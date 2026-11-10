package notify

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{
		Severity:         map[string]string{"critical": SevCritical, "unknown": SevError},
		For:              2 * time.Minute,
		RecoverAfter:     time.Minute,
		GoneAfter:        5 * time.Minute,
		DaemonStaleAfter: 2 * time.Minute,
	}
}

func newTestTracker(p Policy) *Tracker {
	n := 0
	return NewTracker(p, func() string { n++; return fmt.Sprintf("id-%d", n) })
}

var (
	deploy = Key{Cluster: "alpha", Kind: "deployment", Namespace: "shop", Name: "web"}
	clu    = Key{Cluster: "alpha", Kind: KindCluster, Name: "alpha"}
)

func healthy() []Cluster {
	return []Cluster{{Name: "alpha", HeartbeatAge: time.Second, APIReachable: true}}
}

func snap(at time.Duration, clusters []Cluster, objs ...Object) Snapshot {
	return Snapshot{Time: t0.Add(at), Clusters: clusters, Objects: objs}
}

func obj(k Key, state string) Object {
	return Object{Key: k, UUID: "u", State: state, Reason: k.title() + " is " + state + ".\ndetails"}
}

// run evaluates and commits everything, like a poll with a working Notifications API.
func run(tr *Tracker, s Snapshot) []Event {
	ev := tr.Evaluate(s)
	for _, e := range ev {
		tr.Commit(e)
	}
	return ev
}

func wantEvents(t *testing.T, got []Event, want ...string) {
	t.Helper()
	var have []string
	for _, e := range got {
		sev := e.Severity
		if sev == SevNone {
			sev = "recovery"
		}
		have = append(have, e.Key.Kind+"/"+e.Key.Name+"="+sev)
	}
	if fmt.Sprint(have) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", have, want)
	}
}

func TestProblemIsHeldForThenReportedOnce(t *testing.T) {
	tr := newTestTracker(testPolicy())
	wantEvents(t, run(tr, snap(0, healthy(), obj(deploy, "critical"))))
	wantEvents(t, run(tr, snap(119*time.Second, healthy(), obj(deploy, "critical"))))
	ev := run(tr, snap(120*time.Second, healthy(), obj(deploy, "critical")))
	wantEvents(t, ev, "deployment/web=critical")
	if ev[0].Summary != "deployment shop/web is critical." || ev[0].Message == ev[0].Summary {
		t.Fatalf("summary/message not split: %q / %q", ev[0].Summary, ev[0].Message)
	}
	wantEvents(t, run(tr, snap(10*time.Minute, healthy(), obj(deploy, "critical"))))
}

func TestFlappingObjectNeverReports(t *testing.T) {
	// A crash-looping pod alternates ok/critical faster than the hold time.
	tr := newTestTracker(testPolicy())
	for i := 0; i < 40; i++ {
		state := "critical"
		if i%4 == 3 {
			state = "ok"
		}
		wantEvents(t, run(tr, snap(time.Duration(i)*30*time.Second, healthy(), obj(deploy, state))))
	}
}

func TestRecoveryIsHeldAndFlapBackKeepsIncidentOpen(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	wantEvents(t, run(tr, snap(0, healthy(), obj(deploy, "critical"))), "deployment/web=critical")
	wantEvents(t, run(tr, snap(30*time.Second, healthy(), obj(deploy, "ok"))))
	// Back to critical before recover_after: no recovery, no second problem event.
	wantEvents(t, run(tr, snap(60*time.Second, healthy(), obj(deploy, "critical"))))
	wantEvents(t, run(tr, snap(90*time.Second, healthy(), obj(deploy, "ok"))))
	wantEvents(t, run(tr, snap(149*time.Second, healthy(), obj(deploy, "ok"))))
	wantEvents(t, run(tr, snap(150*time.Second, healthy(), obj(deploy, "ok"))), "deployment/web=recovery")
	wantEvents(t, run(tr, snap(200*time.Second, healthy(), obj(deploy, "ok"))))
	if tr.OpenProblems() != 0 {
		t.Fatalf("open problems = %d", tr.OpenProblems())
	}
}

func TestUnmappedStatesAreNoProblemAndPendingIsNoVerdict(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	wantEvents(t, run(tr, snap(0, healthy(), obj(deploy, "warning"))))
	wantEvents(t, run(tr, snap(time.Second, healthy(), obj(deploy, "unknown"))), "deployment/web=error")
	// pending must not recover an open problem, however long it lasts.
	wantEvents(t, run(tr, snap(time.Hour, healthy(), obj(deploy, "pending"))))
	// warning is unmapped, so it counts as healthy and recovers after the hold.
	wantEvents(t, run(tr, snap(2*time.Hour, healthy(), obj(deploy, "warning"))))
	wantEvents(t, run(tr, snap(2*time.Hour+time.Minute, healthy(), obj(deploy, "warning"))), "deployment/web=recovery")
}

func TestSeverityChangeIsReported(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	wantEvents(t, run(tr, snap(0, healthy(), obj(deploy, "unknown"))), "deployment/web=error")
	wantEvents(t, run(tr, snap(time.Second, healthy(), obj(deploy, "critical"))), "deployment/web=critical")
}

func TestUncommittedEventIsRetriedWithSameID(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	first := tr.Evaluate(snap(0, healthy(), obj(deploy, "critical")))
	second := tr.Evaluate(snap(15*time.Second, healthy(), obj(deploy, "critical")))
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Fatalf("retry must reuse the ID: %+v vs %+v", first, second)
	}
	tr.Commit(second[0])
	wantEvents(t, tr.Evaluate(snap(30*time.Second, healthy(), obj(deploy, "critical"))))

	// The recovery is a different transition and must get a different ID.
	rec := tr.Evaluate(snap(time.Hour, healthy(), obj(deploy, "ok")))
	rec = tr.Evaluate(snap(time.Hour+time.Minute, healthy(), obj(deploy, "ok")))
	if len(rec) != 1 || rec[0].ID == first[0].ID {
		t.Fatalf("recovery must have its own ID: %+v", rec)
	}
}

func TestDeletedObjectClosesAfterGoneAfter(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	run(tr, snap(0, healthy(), obj(deploy, "critical")))
	wantEvents(t, run(tr, snap(time.Minute, healthy())))
	wantEvents(t, run(tr, snap(5*time.Minute+59*time.Second, healthy())))
	ev := run(tr, snap(6*time.Minute, healthy()))
	wantEvents(t, ev, "deployment/web=recovery")
	if ev[0].Summary != "deployment shop/web no longer exists" {
		t.Fatalf("summary = %q", ev[0].Summary)
	}
	wantEvents(t, run(tr, snap(7*time.Minute, healthy())))
}

func TestObjectReappearingCancelsRemoval(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	run(tr, snap(0, healthy(), obj(deploy, "critical")))
	run(tr, snap(time.Minute, healthy()))
	run(tr, snap(4*time.Minute, healthy(), obj(deploy, "critical")))
	// Missing again: the clock must start over.
	wantEvents(t, run(tr, snap(5*time.Minute, healthy())))
	wantEvents(t, run(tr, snap(9*time.Minute, healthy())))
	wantEvents(t, run(tr, snap(10*time.Minute, healthy())), "deployment/web=recovery")
}

func TestDaemonRebuildDoesNotCloseOrReopenIncidents(t *testing.T) {
	// The daemon drops its schema on restart: clusters and objects vanish for a moment.
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	run(tr, snap(0, healthy(), obj(deploy, "critical")))
	wantEvents(t, run(tr, snap(time.Minute, nil)))          // tables empty
	wantEvents(t, run(tr, snap(90*time.Second, healthy()))) // cluster back, objects not yet
	wantEvents(t, run(tr, snap(2*time.Minute, healthy(), obj(deploy, "critical"))))
	if tr.OpenProblems() != 1 {
		t.Fatalf("open problems = %d, want 1", tr.OpenProblems())
	}
}

func TestSilentDaemonRaisesClusterIncidentAndFreezesObjects(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	run(tr, snap(0, healthy(), obj(deploy, "critical")))
	stale := func(age time.Duration) []Cluster {
		return []Cluster{{Name: "alpha", HeartbeatAge: age, APIReachable: true}}
	}
	// Stale data says "ok" - it must not recover the deployment.
	wantEvents(t, run(tr, snap(time.Minute, stale(time.Minute), obj(deploy, "ok"))))
	ev := run(tr, snap(3*time.Minute, stale(3*time.Minute), obj(deploy, "ok")))
	wantEvents(t, ev, "cluster/alpha=critical")
	wantEvents(t, run(tr, snap(30*time.Minute, stale(30*time.Minute))))
	// Daemon back: cluster recovers after recover_after, deployment still open.
	wantEvents(t, run(tr, snap(31*time.Minute, healthy(), obj(deploy, "critical"))))
	wantEvents(t, run(tr, snap(32*time.Minute, healthy(), obj(deploy, "critical"))), "cluster/alpha=recovery")
	if tr.OpenProblems() != 1 {
		t.Fatalf("open problems = %d, want 1", tr.OpenProblems())
	}
}

func TestAPIUnreachableRaisesClusterIncidentOnly(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	down := []Cluster{{Name: "alpha", HeartbeatAge: time.Second, APIReachable: false, DaemonMessage: "dial tcp: timeout"}}
	ev := run(tr, snap(0, down, obj(deploy, "critical")))
	wantEvents(t, ev, "cluster/alpha=critical")
	if ev[0].Summary != "Kubernetes API of cluster alpha is unreachable from Icinga for Kubernetes." {
		t.Fatalf("summary = %q", ev[0].Summary)
	}
	wantEvents(t, run(tr, snap(time.Minute, down, obj(deploy, "critical"))))
}

func TestRestoreSendsRecoveryForIncidentOpenedBeforeRestart(t *testing.T) {
	tr := newTestTracker(testPolicy())
	tr.Restore(deploy, SevCritical)
	// Still critical: no duplicate problem notification.
	wantEvents(t, run(tr, snap(0, healthy(), obj(deploy, "critical"))))
	wantEvents(t, run(tr, snap(time.Hour, healthy(), obj(deploy, "ok"))))
	wantEvents(t, run(tr, snap(time.Hour+time.Minute, healthy(), obj(deploy, "ok"))), "deployment/web=recovery")
}

func TestRestoredClusterThatNeverReportsStaysCritical(t *testing.T) {
	tr := newTestTracker(testPolicy())
	tr.Restore(clu, SevCritical)
	tr.Restore(deploy, SevCritical)
	wantEvents(t, run(tr, snap(0, nil)))
	wantEvents(t, run(tr, snap(time.Hour, nil)))
	if tr.OpenProblems() != 2 {
		t.Fatalf("open problems = %d, want 2", tr.OpenProblems())
	}
}

func TestClustersAreIsolated(t *testing.T) {
	p := testPolicy()
	p.For = 0
	tr := newTestTracker(p)
	other := deploy
	other.Cluster = "beta"
	two := []Cluster{
		{Name: "alpha", HeartbeatAge: time.Second, APIReachable: true},
		{Name: "beta", HeartbeatAge: time.Second, APIReachable: true},
	}
	ev := run(tr, snap(0, two, obj(deploy, "critical"), obj(other, "ok")))
	wantEvents(t, ev, "deployment/web=critical")
	if ev[0].Key.Cluster != "alpha" {
		t.Fatalf("wrong cluster: %+v", ev[0].Key)
	}
	// Same namespace/name in beta breaking must be a second, separate incident.
	ev = run(tr, snap(time.Second, two, obj(deploy, "critical"), obj(other, "critical")))
	wantEvents(t, ev, "deployment/web=critical")
	if ev[0].Key.Cluster != "beta" || tr.OpenProblems() != 2 {
		t.Fatalf("clusters not isolated: %+v open=%d", ev[0].Key, tr.OpenProblems())
	}
}

func TestKeyTagsRoundTrip(t *testing.T) {
	for _, k := range []Key{deploy, clu, {Cluster: "a", Kind: "node", Name: "n1"}} {
		got, ok := KeyFromTags(k.Tags())
		if !ok || got != k {
			t.Fatalf("round trip %+v -> %+v (%v)", k, got, ok)
		}
	}
	if _, ok := KeyFromTags(map[string]string{"host": "x", "service": "y"}); ok {
		t.Fatal("foreign tags must be rejected")
	}
}

func TestSummaryIsTruncatedTo1024Runes(t *testing.T) {
	long := make([]rune, 3000)
	for i := range long {
		long[i] = 'ä'
	}
	s, m := splitReason(string(long))
	if n := len([]rune(s)); n != 1024 {
		t.Fatalf("summary runes = %d", n)
	}
	if len([]rune(m)) != 3000 {
		t.Fatal("message must stay complete")
	}
}

func TestNamespaceFilter(t *testing.T) {
	cases := []struct {
		ns       string
		inc, exc []string
		want     bool
	}{
		{"shop", nil, nil, true},
		{"kube-system", nil, []string{"kube-*"}, false},
		{"shop", []string{"shop", "pay-*"}, nil, true},
		{"pay-eu", []string{"shop", "pay-*"}, nil, true},
		{"other", []string{"shop", "pay-*"}, nil, false},
		{"pay-test", []string{"pay-*"}, []string{"*-test"}, false},
	}
	for _, c := range cases {
		if got := namespaceWanted(c.ns, c.inc, c.exc); got != c.want {
			t.Errorf("namespaceWanted(%q, %v, %v) = %v", c.ns, c.inc, c.exc, got)
		}
	}
}
