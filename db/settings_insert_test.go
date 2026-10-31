package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent is the backbone of auth.password_hash: the caller's GetSetting check may
// fail due to database errors, or it may be queued by concurrent requests (bcrypt takes tens of
// milliseconds to run), so the "only first set" guarantee must fall on the primary key constraint
// rather than the upper-level if judgment.
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// The auth.password_hash is the key that is used to create the password.
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("首次写入应返回 inserted=true")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("键已存在时应返回 inserted=false")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("值被覆盖成 %q，应保持 %q", got, "first")
	}
}
