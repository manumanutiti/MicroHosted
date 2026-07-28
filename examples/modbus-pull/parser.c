/* parser.c — Modbus TCP master parser, SIDE A (inside the microVM).
 *
 * The bare minimum of the pull pattern (docs/iot-edge.md): the VM boots, queries
 * ITS sensor (a Modbus TCP slave), and writes a JSON to stdout. The host
 * collects it over vsock (Exec); the guest never initiates anything toward the
 * host.
 *
 * No dependencies: only POSIX/libc. FC 0x03 (read holding registers) is a single
 * frame. It compiles to a tiny static binary — no python needed in the golden
 * (IoT-4 goal: an ultra-minimal image), and it leaves room to shrink it further
 * if needed.
 *
 *   Build (static, to drop into the Alpine/musl guest):
 *     musl-gcc -static -O2 -o parser parser.c        # or inside Alpine:
 *     apk add gcc musl-dev && gcc -static -O2 -o parser parser.c
 *   Test on the host:
 *     gcc -O2 -o parser parser.c
 *
 *   Usage:  ./parser <ip> <port> [count]
 *
 * Output: ALWAYS a JSON on stdout. exit 0 on success, !=0 on failure. No sensor
 * failure (connection, timeout, Modbus exception, short frame) takes down the
 * VM: everything is translated to a clean error JSON.
 *
 * Register map (agreed with simulator.py):
 *   0: temperature (signed int16, x10 -> C)   2: pressure (uint16, hPa)
 *   1: humidity    (uint16, x10 -> %)         3: reading counter
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <sys/select.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>

#define FC_READ_HOLDING 0x03
#define TIMEOUT_SECS    5

/* Prints an error JSON and exits with !=0. kind is a stable tag
 * (conn_refused, timeout, modbus_exception, bad_frame...) for the orchestrator. */
static int fail(const char *sensor, const char *kind, const char *msg) {
    printf("{\"status\":\"error\",\"sensor\":\"%s\",\"error_kind\":\"%s\","
           "\"error\":\"%s\"}\n", sensor, kind, msg);
    return 1;
}

/* connect() with a timeout: non-blocking + select, so a sensor in a black hole
 * (dead IP, blocked egress) doesn't hang the VM. */
static int connect_timeout(int fd, struct sockaddr_in *addr, int secs) {
    int flags = fcntl(fd, F_GETFL, 0);
    fcntl(fd, F_SETFL, flags | O_NONBLOCK);
    int rc = connect(fd, (struct sockaddr *)addr, sizeof(*addr));
    if (rc == 0) { fcntl(fd, F_SETFL, flags); return 0; }
    if (errno != EINPROGRESS) return -1;

    fd_set wset; FD_ZERO(&wset); FD_SET(fd, &wset);
    struct timeval tv = { secs, 0 };
    rc = select(fd + 1, NULL, &wset, NULL, &tv);
    if (rc <= 0) { errno = (rc == 0) ? ETIMEDOUT : errno; return -1; }

    int soerr = 0; socklen_t len = sizeof(soerr);
    getsockopt(fd, SOL_SOCKET, SO_ERROR, &soerr, &len);
    if (soerr != 0) { errno = soerr; return -1; }
    fcntl(fd, F_SETFL, flags);
    return 0;
}

/* Reads exactly n bytes or fails: recv() may return fewer, and a slave that cuts
 * mid-frame must not hang us (SO_RCVTIMEO bounds the wait). */
static int recv_exact(int fd, unsigned char *buf, int n) {
    int got = 0;
    while (got < n) {
        int r = recv(fd, buf + got, n - got, 0);
        if (r == 0) return -1;            /* connection closed */
        if (r < 0) return -2;             /* error/timeout */
        got += r;
    }
    return 0;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: %s <ip> <port> [count]\n", argv[0]);
        printf("{\"status\":\"error\",\"error_kind\":\"usage\","
               "\"error\":\"usage: parser <ip> <port> [count]\"}\n");
        return 2;
    }
    const char *ip = argv[1];
    int port = atoi(argv[2]);
    int count = (argc > 3) ? atoi(argv[3]) : 4;
    if (count < 1 || count > 125) count = 4;

    char sensor[64];
    snprintf(sensor, sizeof(sensor), "%s:%d", ip, port);

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons((unsigned short)port);
    if (inet_pton(AF_INET, ip, &addr.sin_addr) != 1)
        return fail(sensor, "bad_addr", "invalid IP (numeric IPv4 expected)");

    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) return fail(sensor, "socket", "could not create the socket");

    struct timeval tv = { TIMEOUT_SECS, 0 };
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));

    if (connect_timeout(fd, &addr, TIMEOUT_SECS) < 0) {
        int e = errno; close(fd);
        if (e == ECONNREFUSED)
            return fail(sensor, "conn_refused",
                        "connection refused (slave down or egress blocked)");
        if (e == ETIMEDOUT)
            return fail(sensor, "timeout", "timeout connecting to the slave");
        return fail(sensor, "network", strerror(e));
    }

    /* FC03 request: MBAP(txid,proto=0,len=6,unit=1) + PDU(func,start=0,count) */
    unsigned char req[12] = {
        0x00, 0x01,  0x00, 0x00,  0x00, 0x06,  0x01,
        FC_READ_HOLDING, 0x00, 0x00,
        (unsigned char)(count >> 8), (unsigned char)(count & 0xFF)
    };
    if (send(fd, req, sizeof(req), 0) != (int)sizeof(req)) {
        close(fd);
        return fail(sensor, "network", "failed sending the request");
    }

    /* Response: MBAP(7) + PDU. Read the header, extract the length. */
    unsigned char hdr[7];
    if (recv_exact(fd, hdr, 7) < 0) {
        close(fd);
        return fail(sensor, "timeout", "no response from the slave (timeout)");
    }
    int proto = (hdr[2] << 8) | hdr[3];
    int rlen  = (hdr[4] << 8) | hdr[5];   /* includes unit(1) + PDU */
    if (proto != 0 || rlen < 2 || rlen > 260) {
        close(fd);
        return fail(sensor, "bad_frame", "invalid MBAP header");
    }

    unsigned char body[260];
    if (recv_exact(fd, body, rlen - 1) < 0) {   /* -1: unit already read in hdr */
        close(fd);
        return fail(sensor, "bad_frame", "short frame / connection cut");
    }
    close(fd);

    unsigned char func = body[0];
    if (func & 0x80)   /* Modbus exception response */
        return fail(sensor, "modbus_exception", "the slave replied with an exception");
    if (func != FC_READ_HOLDING)
        return fail(sensor, "bad_frame", "unexpected function in the response");

    int bytecount = body[1];
    if (bytecount != count * 2 || bytecount > rlen - 2)
        return fail(sensor, "bad_frame", "byte count doesn't match count");

    /* Raw registers (uint16 big-endian). */
    unsigned short reg[125];
    for (int i = 0; i < count; i++)
        reg[i] = (body[2 + i * 2] << 8) | body[3 + i * 2];

    /* JSON: raw registers + values decoded per the map. */
    printf("{\"status\":\"ok\",\"sensor\":\"%s\",\"registers\":[", sensor);
    for (int i = 0; i < count; i++)
        printf("%s%u", i ? "," : "", reg[i]);
    printf("],\"values\":{");
    int first = 1;
    if (count > 0) {   /* reg0: temperature signed int16, x10 */
        short t = (short)reg[0];
        printf("\"temperature_c\":%.1f", t / 10.0);
        first = 0;
    }
    if (count > 1)
        printf("%s\"humidity_pct\":%.1f", first ? "" : ",", reg[1] / 10.0), first = 0;
    if (count > 2)
        printf("%s\"pressure_hpa\":%u", first ? "" : ",", reg[2]), first = 0;
    if (count > 3)
        printf("%s\"reading_counter\":%u", first ? "" : ",", reg[3]);
    printf("}}\n");
    return 0;
}
