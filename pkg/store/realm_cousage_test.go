package store

import (
	"testing"
	"time"
)

func partnersByPath(ps []CoUsagePartner) map[string]CoUsagePartner {
	m := make(map[string]CoUsagePartner, len(ps))
	for _, p := range ps {
		m[p.Path] = p
	}
	return m
}

// seedContracts' mainnet world is exactly the shape this query is for:
// alice called one+two, bob called one+lib, carol called one only. So one's
// partners are two and lib, each sharing a single address, and carol
// contributes nothing but still counts toward one's own caller total.
func TestRealmCoUsagePartners(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	got, err := db.RealmCoUsagePartners("mainnet", "gno.land/r/a/one", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RealmCoUsagePartners: %v", err)
	}
	if got.Callers != 3 {
		t.Errorf("subject callers = %d, want 3 (alice, bob, carol)", got.Callers)
	}
	if got.Truncated {
		t.Error("truncated on two partners under a limit of 100")
	}
	if len(got.Partners) != 2 {
		t.Fatalf("got %d partners, want 2: %+v", len(got.Partners), got.Partners)
	}

	by := partnersByPath(got.Partners)
	for _, want := range []struct {
		path    string
		shared  int
		calls   int
		callers int
	}{
		{"gno.land/r/a/two", 1, 1, 1},
		{"gno.land/p/b/lib", 1, 1, 1},
	} {
		p, ok := by[want.path]
		if !ok {
			t.Errorf("missing partner %s", want.path)
			continue
		}
		if p.Shared != want.shared || p.Calls != want.calls || p.Callers != want.callers {
			t.Errorf("%s = shared %d, calls %d, callers %d; want %d/%d/%d",
				want.path, p.Shared, p.Calls, p.Callers, want.shared, want.calls, want.callers)
		}
	}
	// The subject is always the heaviest row in its own result and means
	// nothing, so the query excludes it rather than leaving the caller to.
	if _, ok := by["gno.land/r/a/one"]; ok {
		t.Error("the subject realm appears in its own partner list")
	}
}

// The same path is deployed on two chains, and alice called it on both. An
// aggregate that filtered on path alone would hand pearl's realm mainnet's
// partners.
func TestRealmCoUsagePartnersIsPerNetwork(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	got, err := db.RealmCoUsagePartners("pearl", "gno.land/r/a/one", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RealmCoUsagePartners: %v", err)
	}
	if got.Callers != 1 {
		t.Errorf("pearl callers = %d, want 1 (alice)", got.Callers)
	}
	if len(got.Partners) != 0 {
		t.Errorf("pearl partners = %+v, want none: alice called nothing else on that chain", got.Partners)
	}
}

// A window narrows the caller set as well as the partner set. Cutting after
// alice's call to two but before bob's to lib leaves one caller in range and
// one partner, not three callers and two partners.
func TestRealmCoUsagePartnersHonoursWindow(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)

	// alice: one at +1h, two at +2h. bob: one at +3h, lib at +4h.
	since := base.Add(150 * time.Minute)
	got, err := db.RealmCoUsagePartners("mainnet", "gno.land/r/a/one", since, 0)
	if err != nil {
		t.Fatalf("RealmCoUsagePartners: %v", err)
	}
	if got.Callers != 2 {
		t.Errorf("windowed callers = %d, want 2 (bob and carol; alice's call is before the cutoff)", got.Callers)
	}
	by := partnersByPath(got.Partners)
	if _, ok := by["gno.land/r/a/two"]; ok {
		t.Error("two is a partner under a window that excludes alice's only call to it")
	}
	if p, ok := by["gno.land/p/b/lib"]; !ok {
		t.Errorf("lib missing: bob called one and lib inside the window, partners = %+v", got.Partners)
	} else if p.Shared != 1 {
		t.Errorf("lib shared = %d, want 1", p.Shared)
	}
}

// A realm nobody has called has no partners, and says so without running the
// two scans that would prove it.
func TestRealmCoUsagePartnersUncalledRealm(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	got, err := db.RealmCoUsagePartners("mainnet", "gno.land/r/never/called", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RealmCoUsagePartners: %v", err)
	}
	if got.Callers != 0 || len(got.Partners) != 0 || got.Truncated {
		t.Errorf("uncalled realm = %+v, want zero callers and no partners", got)
	}
}

// Truncation is observed, not inferred: the query asks for limit+1 so that a
// full page can be told from a page that happens to be exactly the limit.
func TestRealmCoUsagePartnersTruncates(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	got, err := db.RealmCoUsagePartners("mainnet", "gno.land/r/a/one", time.Time{}, 1)
	if err != nil {
		t.Fatalf("RealmCoUsagePartners: %v", err)
	}
	if len(got.Partners) != 1 || !got.Truncated {
		t.Errorf("limit 1 over two partners = %d partners, truncated %v; want 1 and true",
			len(got.Partners), got.Truncated)
	}

	// Exactly as many partners as the limit is not truncation.
	got, err = db.RealmCoUsagePartners("mainnet", "gno.land/r/a/one", time.Time{}, 2)
	if err != nil {
		t.Fatalf("RealmCoUsagePartners: %v", err)
	}
	if len(got.Partners) != 2 || got.Truncated {
		t.Errorf("limit 2 over two partners = %d partners, truncated %v; want 2 and false",
			len(got.Partners), got.Truncated)
	}
}
