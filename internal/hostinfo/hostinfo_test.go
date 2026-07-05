package hostinfo

import (
	"os"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	data := `MemTotal:       32620576 kB
MemFree:         1928136 kB
MemAvailable:   24461808 kB
Buffers:         1035140 kB
`
	m, err := parseMeminfo(data)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if m.TotalMB != 32620576/1024 {
		t.Errorf("TotalMB = %d, want %d", m.TotalMB, 32620576/1024)
	}
	if m.AvailableMB != 24461808/1024 {
		t.Errorf("AvailableMB = %d, want %d", m.AvailableMB, 24461808/1024)
	}
	if m.UsedMB != (32620576-24461808)/1024 {
		t.Errorf("UsedMB = %d, want %d", m.UsedMB, (32620576-24461808)/1024)
	}
}

func TestParseMeminfoMissingTotal(t *testing.T) {
	if _, err := parseMeminfo("MemFree: 123 kB\n"); err == nil {
		t.Fatal("expected error for meminfo without MemTotal")
	}
}

func TestParseLoadavg(t *testing.T) {
	l, err := parseLoadavg("0.52 1.10 2.35 2/1874 1067523\n")
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if l.Load1 != 0.52 || l.Load5 != 1.10 || l.Load15 != 2.35 {
		t.Errorf("got %+v", l)
	}
}

func TestParseLoadavgMalformed(t *testing.T) {
	if _, err := parseLoadavg("garbage\n"); err == nil {
		t.Fatal("expected error for malformed loadavg")
	}
}

// TestParseProcStat uses a comm with spaces and parentheses — the case that
// breaks naive whitespace splitting.
func TestParseProcStat(t *testing.T) {
	// pid 42, comm "(fire cracker) x", state S; utime=1200 stime=300 ticks
	// (field 14/15), starttime=500000 ticks (field 22).
	stat := "42 ((fire cracker) x) S 1 42 42 0 -1 4194560 1000 0 0 0 1200 300 0 0 20 0 2 0 500000 123456789 2000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"
	s, err := parseProcStat(stat, 10000) // host up 10000s
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if s.CPUSeconds != 15.0 { // (1200+300)/100
		t.Errorf("CPUSeconds = %v, want 15", s.CPUSeconds)
	}
	if s.UptimeSeconds != 10000-500000/clockTicks {
		t.Errorf("UptimeSeconds = %d, want %d", s.UptimeSeconds, 10000-500000/clockTicks)
	}
}

func TestParseProcStatMalformed(t *testing.T) {
	if _, err := parseProcStat("no parens here", 100); err == nil {
		t.Fatal("expected error without comm parens")
	}
}

// TestReadProcStatsSelf exercises the real /proc path against our own PID.
func TestReadProcStatsSelf(t *testing.T) {
	s, err := ReadProcStats(os.Getpid())
	if err != nil {
		t.Fatalf("ReadProcStats(self): %v", err)
	}
	if s.RSSMB <= 0 {
		t.Errorf("RSSMB = %d, want > 0", s.RSSMB)
	}
	if s.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %d, want >= 0", s.UptimeSeconds)
	}
}

func TestDirSizeMB(t *testing.T) {
	dir := t.TempDir()
	// 4 MiB of real (non-sparse) data.
	if err := os.WriteFile(dir+"/f", make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DirSizeMB(dir); got < 4 {
		t.Errorf("DirSizeMB = %d, want >= 4", got)
	}
	if got := DirSizeMB(dir + "/missing"); got != 0 {
		t.Errorf("DirSizeMB(missing) = %d, want 0", got)
	}
}

func TestReadDiskUsage(t *testing.T) {
	du, err := ReadDiskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("ReadDiskUsage: %v", err)
	}
	if du.TotalMB <= 0 || du.FSType == "" {
		t.Errorf("got %+v", du)
	}
}
