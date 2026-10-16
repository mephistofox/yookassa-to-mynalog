package sqlitestate

import (
	"context"
	"github.com/mephistofox/yookassa-to-mynalog/pkg/receipts"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreLockAndConsistentBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(path, true); err == nil {
		other.Close()
		t.Fatal("second writer acquired state")
	}
	p := PaymentJob{ID: "one", Phase: "done", Receipt: "r1"}
	d := Delivery{ID: "one", Phase: "ready", Receipt: "r1"}
	if err = s.Save(paymentRecord(&p), deliveryRecord(&d)); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	backup := filepath.Join(filepath.Dir(path), "backup.db")
	if err = reader.Backup(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	b, err := Open(backup, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	counts, err := b.Counts()
	if err != nil || counts["payment.done"] != 1 || counts["delivery.ready"] != 1 {
		t.Fatalf("backup incomplete %v %v", counts, err)
	}
	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup permissions are not private")
	}
	if err = reader.Backup(context.Background(), backup); err == nil {
		t.Fatal("existing backup overwritten")
	}
}
func TestStoreRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path, true); err == nil {
		s.Close()
		t.Fatal("followed state symlink")
	}
}
func TestExactMoney(t *testing.T) {
	for _, s := range []string{"0.00", "0.01", "10.10", "2400000.00", "99999999999999.99"} {
		m, err := ParseMoney(s)
		if err != nil || m.String() != s {
			t.Fatalf("%s => %s %v", s, m, err)
		}
	}
	for _, s := range []string{"-1.00", "1e2", "NaN", "1", "1.001", "1.0", ".10", "1,10", " 1.00", "999999999999999.99"} {
		if _, err := ParseMoney(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestPythonStateCannotBeUsedAsGoState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "python-state.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("CREATE TABLE sync_state(id INTEGER PRIMARY KEY,data TEXT)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if next, err := Open(path, true); err == nil {
		next.Close()
		t.Fatal("Python state was silently reused without deduplication migration")
	}
}

type PaymentJob = receipts.PaymentJob
type Delivery = receipts.Delivery

var ParseMoney = receipts.ParseMoney

func paymentRecord(p *PaymentJob) Record {
	return Record{Kind: "payment", ID: p.ID, Phase: p.Phase, Value: p}
}
func deliveryRecord(d *Delivery) Record {
	return Record{Kind: "delivery", ID: d.ID, Phase: d.Phase, Value: d}
}
