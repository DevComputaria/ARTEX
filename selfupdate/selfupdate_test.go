package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnabledRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("ARTEX_SELF_UPDATE", "")
	if Enabled() {
		t.Fatal("a atualização externa deve ficar desativada por padrão")
	}
	t.Setenv("ARTEX_SELF_UPDATE", "true")
	if !Enabled() {
		t.Fatal("ARTEX_SELF_UPDATE=true deve habilitar a atualização externa")
	}
}

// testPaths Creates an isolated upgrade directory. You cannot use ResolvePaths() directly — it
// points to the test binary itself, and renames the executable of go test on run.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin writes an executable shell script that impersonates artex. smokeTest just pulls it with
// -h to see the exit code, the script is perfectly useful and much faster than compiling a real
// binary.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("写入假二进制 %s: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage Make bin look like it is "restored": write artex.new and its checksum.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("计算校验和: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("写入校验和: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("假二进制用的是 sh 脚本，Windows 上跑不了")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // Build.sh Remove v, tag with v, both sides must be recognized
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // By numerical ratio rather than dictionary order
		{"1.0.0", "0.99.99", 1, true},
		// The development build must be deemed uncomparable, otherwise the official version will
		// overwrite the unsubmitted changes.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, 期望 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, 期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// Key invariant: All upgrade files are in the same directory as the executable. Falling into
	// CWD will make the reboot when serviced runs (the working directory may be /) completely
	// invalid.
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s 不在可执行文件目录下: %s (期望 %s)", name, path, p.Dir)
		}
	}
	// On Windows,.new/.old must retain.exe, otherwise both smoke testing and reinstallation
	// execution will fail.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows 上 .new/.old 必须以 .exe 结尾: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// After verification and writing, modify the file to simulate a corrupted / unpacked download.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("期望 SHA256 不匹配被拒绝，却通过了")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // Execute but exit code is not 0

	if err := verifyStaged(p); err == nil {
		t.Fatal("期望冒烟测试失败被拒绝，却通过了")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("写标记: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("期望 Restart，得到 %v", action)
	}
	if !st.Pending {
		t.Error("换装后状态应为 Pending")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex 应已被替换为新版本")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧版本应备份到 artex.old")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("换装后 artex.new 应已消失")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("换装后校验和文件应已清理")
	}
	// The tag must be kept, and the next time you start (if it is a new version) it will count, and
	// roll back if necessary.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("换装后升级标记应保留")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // and

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("校验失败时期望 Continue，得到 %v", action)
	}
	if !st.FailedStage {
		t.Error("状态应标记为 FailedStage")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("校验失败时绝不能动当前版本")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("校验失败的暂存件应被清理，否则下次启动会再试一遍")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // Backup left from the last upgrade
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("应换装到 v3")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("备份应更新为刚被换下的 v2")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// The previous maxAttempts starts are only cumulative, giving the new version a chance to
	// settle.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("第 %d 次尝试期望 Continue，得到 %v", i, action)
		}
		if !st.Pending {
			t.Errorf("第 %d 次尝试状态应为 Pending", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("第 %d 次尝试后 attempts=%d（ok=%v），期望 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// If you crash again, the limit is exceeded and the old version is automatically replaced.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("超过尝试上限时期望 Restart，得到 %v", action)
	}
	if !st.RolledBack {
		t.Error("状态应标记为 RolledBack")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("应已回滚到旧版本")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("回滚后标记应清除，否则会无限回滚")
	}
	// The version that does not work is kept for checks and not deleted directly.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("失败的版本应保留为 .failed 供排查")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() Goes to ResolvePaths(), which directly tests the underlying layer's interchange
	// semantics.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("回滚后当前版本应是 v1")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("回滚后备份应变成 v2，这样还能再滚回去")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum output is double space separated; shasum -a 256 adds a * to the filename in binary
	// mode.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // Exactly two fields, but the first is not an abstract
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // The summary length is incorrect

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux 条目解析错误: %v", out)
	}
	// The summaries should be in lowercase, so that they are not mistakenly considered to be
	// inconsistent due to case errors.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows 条目错误（* 前缀应剥离、摘要应转小写）: %q", got)
	}
	if len(out) != 2 {
		t.Errorf("应忽略空行、非摘要行和长度不对的行，得到 %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("包内基名在 Windows 上是 artex.exe，此用例按 Unix 命名构造")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// The actual distribution package structure is: artex-<version>-<os>-<arch>/artex, plus some
	// interference files.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("解压出来的不是 artex 可执行文件: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("解压出的二进制必须带执行位")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("包内没有可执行文件时应报错")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // Non-HTTPS
		"https://evil.com/artex.zip",    // Domain Names in the White List
		"https://github.com.evil.com/x", // Suffix disguise
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) 应当拒绝", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // Domain Names
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) 应当放行，却报错: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// The package_binary in build.sh uses artex-<version>-<os>-<arch>.zip, and the version number
	// removes the v prefix.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 %q: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("确认稳定后升级标记必须清除")
	}
	// When the marker is gone, subsequent normal restarts do not accumulate any more times or
	// inadvertently trigger rollbacks.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("标记读取应失败")
	}
	// The backup is kept and the user can roll back manually.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("确认稳定后仍应保留上一版本备份")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // Normal boot path, no panic, no files moved
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("无标记时 settle 不应影响任何文件")
	}
}
