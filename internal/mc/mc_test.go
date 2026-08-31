package mc

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMinecraftDir points MinecraftDir at a temp .minecraft by overriding
// %AppData% (os.UserConfigDir on Windows reads that exact env var).
func fakeMinecraftDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("APPDATA", base)
	mc := filepath.Join(base, ".minecraft")
	if err := os.MkdirAll(mc, 0o755); err != nil {
		t.Fatal(err)
	}
	return mc
}

func TestMinecraftDirHonorsAppData(t *testing.T) {
	mc := fakeMinecraftDir(t)
	if got := MinecraftDir(); got != mc {
		t.Fatalf("MinecraftDir = %q, want %q", got, mc)
	}
	if got := MinecraftDir(); got != mc {
		t.Fatal("MinecraftDir changed between calls")
	}
}

func TestLauncherExe(t *testing.T) {
	mc := fakeMinecraftDir(t)
	launcher := filepath.Join(mc, "launcher", "MinecraftLauncher.exe")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LauncherExe(); got != launcher {
		t.Fatalf("LauncherExe = %q, want %q", got, launcher)
	}
}

func TestFindJavaUnderNested(t *testing.T) {
	root := t.TempDir()
	java := filepath.Join(root, "runtime", "jre-legacy", "windows-x64", "jre", "bin", "java.exe")
	if err := os.MkdirAll(filepath.Dir(java), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(java, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findJavaUnder(root); got != java {
		t.Fatalf("findJavaUnder = %q, want %q", got, java)
	}
	// A java.exe deeper than the depth bound must not be returned (avoids
	// pathological full-disk walks).
	deep := filepath.Join(root, "a", "b", "c", "d", "e", "f", "g", "bin", "java.exe")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deep, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findJavaUnder(root); got != java {
		t.Fatalf("deep java.exe leaked past the depth bound: %q", got)
	}
}

func TestPrepareServerDir(t *testing.T) {
	dir := t.TempDir()
	if err := PrepareServerDir(dir); err != nil {
		t.Fatal(err)
	}
	eula, err := os.ReadFile(filepath.Join(dir, "eula.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(eula), "eula=true") {
		t.Fatalf("eula.txt = %q, want eula=true", eula)
	}
	props, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(props)
	for _, want := range []string{"server-port=25565", "online-mode=false", "motd=Eliauk VPN"} {
		if !strings.Contains(text, want) {
			t.Fatalf("server.properties missing %q:\n%s", want, text)
		}
	}

	// A second run must be idempotent and must preserve user-chosen keys that
	// are not part of our defaults.
	if err := os.WriteFile(filepath.Join(dir, "server.properties"),
		[]byte("server-port=19132\nmax-players=4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareServerDir(dir); err != nil {
		t.Fatal(err)
	}
	props, _ = os.ReadFile(filepath.Join(dir, "server.properties"))
	text = string(props)
	if !strings.Contains(text, "server-port=25565") {
		t.Fatalf("server-port not reset to default:\n%s", text)
	}
	if !strings.Contains(text, "max-players=4") {
		t.Fatalf("existing max-players lost:\n%s", text)
	}
	if !strings.Contains(text, "online-mode=false") {
		t.Fatalf("online-mode missing:\n%s", text)
	}
}

func TestNBTRoundTrip(t *testing.T) {
	// A realistic servers.dat tree: root compound with a servers list holding
	// two entries, one of which carries the extra optional tags the launcher
	// writes (icon byte array, resource pack, lastPlayed long).
	entry := func(name, ip string) *nbtNode {
		return &nbtNode{typ: tagCompound, val: []*nbtNode{
			{name: "name", typ: tagString, val: name},
			{name: "ip", typ: tagString, val: ip},
			{name: "icon", typ: tagByteArray, val: []byte{0x89, 0x50, 0x4e}},
			{name: "lastPlayed", typ: tagLong, val: int64(1700000000000)},
			{name: "acceptTextures", typ: tagByte, val: int8(1)},
		}}
	}
	root := &nbtNode{typ: tagCompound, val: []*nbtNode{
		{name: "servers", typ: tagList, val: []*nbtNode{
			entry("One", "1.1.1.1:25565"),
			entry("Two", "2.2.2.2"),
		}},
	}}

	back, err := roundTripNBT(root)
	if err != nil {
		t.Fatal(err)
	}
	if back.typ != tagCompound {
		t.Fatalf("root typ = 0x%02x", back.typ)
	}
	servers := back.child("servers")
	if servers == nil {
		t.Fatal("servers list missing after round-trip")
	}
	list := servers.val.([]*nbtNode)
	if len(list) != 2 {
		t.Fatalf("servers len = %d, want 2", len(list))
	}
	if list[0].stringOf("name") != "One" || list[0].stringOf("ip") != "1.1.1.1:25565" {
		t.Fatalf("entry 0 corrupted: %+v", list[0])
	}
	if lp := list[1].child("lastPlayed"); lp == nil || lp.typ != tagLong {
		t.Fatal("lastPlayed long tag lost")
	}
	icon, ok := list[1].child("icon").val.([]byte)
	if !ok || len(icon) != 3 {
		t.Fatal("icon byte array lost")
	}
}

func TestAddServerToLauncher(t *testing.T) {
	mc := fakeMinecraftDir(t)

	// Fresh install: no servers.dat -> entry created.
	added, err := addServerToLauncherIn(mc, "Eliauk 房主", "10.8.0.1:25565")
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("expected change on first add")
	}
	if _, err := os.Stat(filepath.Join(mc, "servers.dat")); err != nil {
		t.Fatalf("servers.dat not written: %v", err)
	}

	// Add a second entry.
	if _, err := addServerToLauncherIn(mc, "友服", "10.8.0.2:25565"); err != nil {
		t.Fatal(err)
	}

	// Re-adding the same address updates in place — no duplicate row.
	added, err = addServerToLauncherIn(mc, "Eliauk 房主 (新)", "10.8.0.1:25565")
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("expected update when address re-added")
	}
	root, exists, err := readServersDat(filepath.Join(mc, "servers.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("servers.dat should exist")
	}
	list := root.child("servers").val.([]*nbtNode)
	if len(list) != 2 {
		t.Fatalf("servers len = %d, want 2 (no duplicates)", len(list))
	}
	if list[0].stringOf("ip") != "10.8.0.1:25565" || list[0].stringOf("name") != "Eliauk 房主 (新)" {
		t.Fatalf("entry 0 not updated: %+v", list[0])
	}

	// Backup was created.
	if _, err := os.Stat(filepath.Join(mc, "servers.dat.bak")); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestAddServerToLauncherPreservesExistingEntries(t *testing.T) {
	mc := fakeMinecraftDir(t)
	// Seed a servers.dat with a pre-existing server and extra root tags.
	root := &nbtNode{typ: tagCompound, val: []*nbtNode{
		{name: "servers", typ: tagList, val: []*nbtNode{
			{typ: tagCompound, val: []*nbtNode{
				{name: "name", typ: tagString, val: "Hypixel"},
				{name: "ip", typ: tagString, val: "mc.hypixel.net"},
			}},
		}},
		{name: "meta", typ: tagInt, val: int32(7)},
	}}
	if err := writeServersDat(filepath.Join(mc, "servers.dat"), root); err != nil {
		t.Fatal(err)
	}
	if _, err := addServerToLauncherIn(mc, "房主", "10.8.0.1:25565"); err != nil {
		t.Fatal(err)
	}
	root, _, err := readServersDat(filepath.Join(mc, "servers.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if meta := root.child("meta"); meta == nil || meta.val != int32(7) {
		t.Fatal("unrelated root tag lost")
	}
	list := root.child("servers").val.([]*nbtNode)
	if len(list) != 2 {
		t.Fatalf("servers len = %d, want 2", len(list))
	}
	if list[0].stringOf("name") != "Hypixel" {
		t.Fatalf("pre-existing entry corrupted: %+v", list[0])
	}
}

// helpers ---------------------------------------------------------------

func roundTripNBT(root *nbtNode) (*nbtNode, error) {
	var b bytes.Buffer
	if err := writeNBT(&b, root); err != nil {
		return nil, err
	}
	return readNBT(bufio.NewReader(&b))
}

func writeServersDat(path string, root *nbtNode) error {
	var b bytes.Buffer
	if err := writeNBT(&b, root); err != nil {
		return err
	}
	// Real servers.dat is gzip-compressed; readServersDat expects that.
	var comp bytes.Buffer
	gw := gzip.NewWriter(&comp)
	if _, err := gw.Write(b.Bytes()); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, comp.Bytes(), 0o644)
}
