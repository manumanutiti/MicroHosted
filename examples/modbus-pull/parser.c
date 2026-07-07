/* parser.c — parser Modbus TCP maestro, LADO A (dentro de la microVM).
 *
 * Lo mínimo indispensable del patrón pull (docs/iot-edge.md): la VM arranca,
 * pregunta a SU sensor (slave Modbus TCP), y escribe un JSON por stdout. El
 * host lo recoge por vsock (Exec); el guest nunca inicia nada hacia el host.
 *
 * Sin dependencias: solo POSIX/libc. FC 0x03 (read holding registers) es una
 * sola trama. Compila a un binario estático diminuto — no hace falta python en
 * el golden (objetivo IoT-4: imagen ultra-mínima), y deja el camino a bajarlo
 * aún más si hiciera falta.
 *
 *   Compilar (estático, para meter en el guest Alpine/musl):
 *     musl-gcc -static -O2 -o parser parser.c        # o dentro de Alpine:
 *     apk add gcc musl-dev && gcc -static -O2 -o parser parser.c
 *   Probar en el host:
 *     gcc -O2 -o parser parser.c
 *
 *   Uso:  ./parser <ip> <puerto> [count]
 *
 * Salida: SIEMPRE un JSON en stdout. exit 0 en éxito, !=0 en fallo. Ningún
 * fallo del sensor (conexión, timeout, excepción Modbus, trama corta) tumba la
 * VM: todo se traduce a un JSON de error limpio.
 *
 * Mapa de registros (acordado con simulator.py):
 *   0: temperatura (int16 con signo, x10 -> C)   2: presion (uint16, hPa)
 *   1: humedad     (uint16, x10 -> %)            3: contador de lecturas
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

/* Imprime un JSON de error y sale con !=0. kind es una etiqueta estable
 * (conn_refused, timeout, modbus_exception, bad_frame...) para el orquestador. */
static int fail(const char *sensor, const char *kind, const char *msg) {
    printf("{\"status\":\"error\",\"sensor\":\"%s\",\"error_kind\":\"%s\","
           "\"error\":\"%s\"}\n", sensor, kind, msg);
    return 1;
}

/* connect() con timeout: no-bloqueante + select, para que un sensor en un
 * agujero negro (IP muerta, egress bloqueado) no cuelgue la VM. */
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

/* Lee exactamente n bytes o falla: recv() puede devolver menos, y un slave que
 * corta a media trama no debe colgarnos (el SO_RCVTIMEO acota la espera). */
static int recv_exact(int fd, unsigned char *buf, int n) {
    int got = 0;
    while (got < n) {
        int r = recv(fd, buf + got, n - got, 0);
        if (r == 0) return -1;            /* conexión cerrada */
        if (r < 0) return -2;             /* error/timeout */
        got += r;
    }
    return 0;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "uso: %s <ip> <puerto> [count]\n", argv[0]);
        printf("{\"status\":\"error\",\"error_kind\":\"usage\","
               "\"error\":\"uso: parser <ip> <puerto> [count]\"}\n");
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
        return fail(sensor, "bad_addr", "IP invalida (se espera IPv4 numerica)");

    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) return fail(sensor, "socket", "no se pudo crear el socket");

    struct timeval tv = { TIMEOUT_SECS, 0 };
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));

    if (connect_timeout(fd, &addr, TIMEOUT_SECS) < 0) {
        int e = errno; close(fd);
        if (e == ECONNREFUSED)
            return fail(sensor, "conn_refused",
                        "conexion rechazada (slave caido o egress bloqueado)");
        if (e == ETIMEDOUT)
            return fail(sensor, "timeout", "timeout conectando al slave");
        return fail(sensor, "network", strerror(e));
    }

    /* Petición FC03: MBAP(txid,proto=0,len=6,unit=1) + PDU(func,start=0,count) */
    unsigned char req[12] = {
        0x00, 0x01,  0x00, 0x00,  0x00, 0x06,  0x01,
        FC_READ_HOLDING, 0x00, 0x00,
        (unsigned char)(count >> 8), (unsigned char)(count & 0xFF)
    };
    if (send(fd, req, sizeof(req), 0) != (int)sizeof(req)) {
        close(fd);
        return fail(sensor, "network", "fallo enviando la peticion");
    }

    /* Respuesta: MBAP(7) + PDU. Leemos la cabecera, sacamos la longitud. */
    unsigned char hdr[7];
    if (recv_exact(fd, hdr, 7) < 0) {
        close(fd);
        return fail(sensor, "timeout", "sin respuesta del slave (timeout)");
    }
    int proto = (hdr[2] << 8) | hdr[3];
    int rlen  = (hdr[4] << 8) | hdr[5];   /* incluye unit(1) + PDU */
    if (proto != 0 || rlen < 2 || rlen > 260) {
        close(fd);
        return fail(sensor, "bad_frame", "cabecera MBAP invalida");
    }

    unsigned char body[260];
    if (recv_exact(fd, body, rlen - 1) < 0) {   /* -1: unit ya leido en hdr */
        close(fd);
        return fail(sensor, "bad_frame", "trama corta / conexion cortada");
    }
    close(fd);

    unsigned char func = body[0];
    if (func & 0x80)   /* respuesta de excepcion Modbus */
        return fail(sensor, "modbus_exception", "el slave respondio excepcion");
    if (func != FC_READ_HOLDING)
        return fail(sensor, "bad_frame", "funcion inesperada en la respuesta");

    int bytecount = body[1];
    if (bytecount != count * 2 || bytecount > rlen - 2)
        return fail(sensor, "bad_frame", "byte count no cuadra con count");

    /* Registros crudos (uint16 big-endian). */
    unsigned short reg[125];
    for (int i = 0; i < count; i++)
        reg[i] = (body[2 + i * 2] << 8) | body[3 + i * 2];

    /* JSON: registros crudos + valores decodificados segun el mapa. */
    printf("{\"status\":\"ok\",\"sensor\":\"%s\",\"registers\":[", sensor);
    for (int i = 0; i < count; i++)
        printf("%s%u", i ? "," : "", reg[i]);
    printf("],\"values\":{");
    int first = 1;
    if (count > 0) {   /* reg0: temperatura int16 con signo, x10 */
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
