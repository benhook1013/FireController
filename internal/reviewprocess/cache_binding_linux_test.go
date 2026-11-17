package reviewprocess

import (
	"context"
	"os"
	"testing"
)

func TestDraftBindingShape(t *testing.T) {
	for _, s := range []string{`{"version":1,"version":1}`, `{"a":{"x":1,"x":2}}`, `{"a":[]}
`, `{"version":1} {}`} {
		if uniqueBindingJSON([]byte(s)) == nil {
			t.Fatalf("accepted ambiguous shape %s", s)
		}
	}
	valid := `{"version":1,"state":"enabled","policy":"exclusive-home-v1","boot_id":"boot","cgroup":"group","source":"source","configuration_sha256":"config","owner_evidence_sha256":"owner","home":"/home","home_device":1,"home_inode":2,"home_uid":3,"home_gid":4}`
	if err := uniqueBindingJSON([]byte(valid)); err != nil {
		t.Fatal(err)
	}
}
func TestDraftBindingRejectsCaseAliases(t *testing.T) {
	for _, s := range []string{`{"source":"one","Source":"two"}`, `{"state":"enabled","State":"disabled"}`, `{"Version":1}`, `{"version":1}`} {
		if uniqueBindingJSON([]byte(s)) == nil {
			t.Fatal("ambiguous or incomplete binding accepted", s)
		}
	}
}

func TestDraftBindingConstructor(t *testing.T) {
	source := "1234567890123456789012345678901234567890"
	config := "1234567890123456789012345678901234567890123456789012345678901234"
	if _, err := NewCacheMaintenanceBinding("/binding/adoption.json", source, config, "/account"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"relative", "/x/../binding", "/binding/"} {
		if _, err := NewCacheMaintenanceBinding(p, source, config, "/account"); err == nil {
			t.Fatal(p)
		}
	}
}

func TestDraftDirectoryProjection(t *testing.T) {
	good := []byte("1 0 0:1 /owner /binding ro,relatime - ext4 /dev/root ro\n")
	if !bindingDirectoryProjection(good, "/binding/adoption.json") {
		t.Fatal("directory denied")
	}
	for _, bad := range []string{"1 0 0:1 / / ro - ext4 /dev/root ro\n", "1 0 0:1 /owner /binding rw - ext4 /dev/root rw\n", string(good) + "2 1 0:1 /old /binding/adoption.json ro - ext4 /dev/root ro\n"} {
		if bindingDirectoryProjection([]byte(bad), "/binding/adoption.json") {
			t.Fatal("unsafe projection accepted")
		}
	}
}

func TestDraftBindingNamespaceProbe(t *testing.T) {
	if os.Getenv("FC_BINDING_PROBE") != "1" {
		t.Skip("explicit disposable namespace only")
	}
	b, err := NewCacheMaintenanceBinding(os.Getenv("FC_BINDING_PATH"), os.Getenv("FC_BINDING_SOURCE"), os.Getenv("FC_BINDING_CONFIG"), os.Getenv("FC_BINDING_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	lease, leaseErr := NewCacheHomeLease(os.Getenv("FC_BINDING_HOME"))
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	defer lease.Close()
	r := &Runner{cacheCustody: &CacheCustody{}, homeLease: lease, env: []string{"HOME=" + os.Getenv("FC_BINDING_HOME")}}
	called := false
	err = r.WithAdoptedCacheMaintenance(context.Background(), b, func(home string) error { called = true; return nil })
	if (err == nil) != called {
		t.Fatal("callback/authority disagreement", err, called)
	}
	if os.Getenv("FC_BINDING_REFUSE") == "1" {
		if err == nil {
			t.Fatal("invalid binding accepted")
		}
	} else if err != nil {
		t.Fatal(err)
	}
}
