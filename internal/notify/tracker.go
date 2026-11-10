// Package notify turns the object states computed by Icinga for Kubernetes into
// Icinga Notifications events.
package notify

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severities understood by Icinga Notifications. The empty string means "no problem".
const (
	SevNone     = ""
	SevWarning  = "warning"
	SevError    = "error"
	SevCritical = "critical"
)

// KindCluster is the pseudo kind used for incidents about a cluster as a whole
// (daemon silent, Kubernetes API unreachable).
const KindCluster = "cluster"

// Key identifies a monitored object. It uses names instead of the daemon's UUIDs so
// that it stays stable when the daemon rebuilds its database.
type Key struct {
	Cluster   string
	Kind      string
	Namespace string
	Name      string
}

func (k Key) String() string {
	return k.Cluster + ": " + k.title()
}

func (k Key) title() string {
	if k.Namespace == "" {
		return k.Kind + " " + k.Name
	}
	return k.Kind + " " + k.Namespace + "/" + k.Name
}

// IntegrationTag marks every object of this integration. Icinga Notifications can only
// list incidents by a positive tag match, so this is what a restart searches for.
const (
	IntegrationTag      = "integration"
	IntegrationTagValue = "kubernetes"
)

// Tags are the identifying tags of the object in Icinga Notifications.
func (k Key) Tags() map[string]string {
	t := map[string]string{IntegrationTag: IntegrationTagValue, "cluster": k.Cluster, "kind": k.Kind, "name": k.Name}
	if k.Namespace != "" {
		t["namespace"] = k.Namespace
	}
	return t
}

// KeyFromTags is the inverse of Key.Tags. ok is false if the tags are not ours.
func KeyFromTags(t map[string]string) (Key, bool) {
	k := Key{Cluster: t["cluster"], Kind: t["kind"], Namespace: t["namespace"], Name: t["name"]}
	return k, t[IntegrationTag] == IntegrationTagValue && k.Cluster != "" && k.Kind != "" && k.Name != ""
}

// Object is one row read from the Icinga for Kubernetes database.
type Object struct {
	Key    Key
	UUID   string // daemon UUID, only used to build links
	State  string // icinga_state: ok, warning, critical, unknown, pending
	Reason string // icinga_state_reason
}

// Cluster is the health of one Icinga for Kubernetes daemon instance.
type Cluster struct {
	Name          string
	HeartbeatAge  time.Duration
	APIReachable  bool
	DaemonMessage string
}

// Snapshot is everything read in one poll.
type Snapshot struct {
	Time     time.Time
	Clusters []Cluster
	Objects  []Object
}

// Event is what gets submitted to Icinga Notifications.
type Event struct {
	ID       string // stable across retries of the same transition
	Key      Key
	UUID     string
	Severity string // SevNone means recovery
	Summary  string
	Message  string
}

// Policy controls when a state becomes an event.
type Policy struct {
	// Severity maps an icinga_state to a notification severity. States that are missing
	// or map to "" are treated as "no problem".
	Severity map[string]string
	// For is how long a problem must persist before it is reported.
	For time.Duration
	// RecoverAfter is how long an object must be healthy before recovery is reported.
	RecoverAfter time.Duration
	// GoneAfter is how long an object with an open problem must be absent before the
	// problem is closed.
	GoneAfter time.Duration
	// DaemonStaleAfter is the heartbeat age after which a cluster's data is distrusted.
	DaemonStaleAfter time.Duration
}

type record struct {
	reported     string // severity Icinga Notifications currently has for this object
	candidate    string
	candidateSet bool
	since        time.Time
	missingSince time.Time
	pendingID    string
	uuid         string
}

// Tracker is the state machine. It is not safe for concurrent use.
type Tracker struct {
	policy   Policy
	records  map[Key]*record
	lastSeen map[string]time.Time // cluster -> last time its heartbeat was fresh (zero: never)
	newID    func() string
}

func NewTracker(p Policy, newID func() string) *Tracker {
	return &Tracker{policy: p, records: map[Key]*record{}, lastSeen: map[string]time.Time{}, newID: newID}
}

// Restore seeds the tracker with an incident that is already open in Icinga Notifications,
// so a restart neither repeats the problem notification nor forgets to send the recovery.
func (t *Tracker) Restore(k Key, severity string) {
	t.records[k] = &record{reported: severity}
	if _, ok := t.lastSeen[k.Cluster]; !ok {
		t.lastSeen[k.Cluster] = time.Time{}
	}
}

// Commit records that an event was accepted by Icinga Notifications.
func (t *Tracker) Commit(e Event) {
	r, ok := t.records[e.Key]
	if !ok || r.pendingID != e.ID {
		return
	}
	if e.Severity == SevNone {
		delete(t.records, e.Key)
		return
	}
	r.reported = e.Severity
	r.pendingID = ""
	r.candidateSet = false
}

// OpenProblems returns how many objects currently have a reported problem.
func (t *Tracker) OpenProblems() int {
	n := 0
	for _, r := range t.records {
		if r.reported != SevNone {
			n++
		}
	}
	return n
}

// Evaluate consumes a snapshot and returns the events that are due. An event is returned
// again on every call until it is committed, always with the same ID.
func (t *Tracker) Evaluate(s Snapshot) []Event {
	var events []Event
	now := s.Time

	// 1. Cluster health decides whether object data can be trusted.
	present := map[string]Cluster{}
	for _, c := range s.Clusters {
		present[c.Name] = c
		if beat := now.Add(-c.HeartbeatAge); beat.After(t.lastSeen[c.Name]) {
			t.lastSeen[c.Name] = beat
		}
	}
	names := make([]string, 0, len(t.lastSeen))
	for name := range t.lastSeen {
		names = append(names, name)
	}
	sort.Strings(names)

	trusted := map[string]bool{}
	for _, name := range names {
		seen := t.lastSeen[name]
		key := Key{Cluster: name, Kind: KindCluster, Name: name}
		c, here := present[name]
		var (
			desired = SevNone
			hold    = t.policy.RecoverAfter
			reason  = fmt.Sprintf("Icinga for Kubernetes is reporting for cluster %s and the Kubernetes API is reachable.", name)
		)
		switch {
		case seen.IsZero():
			desired, hold = SevCritical, t.policy.For
			reason = fmt.Sprintf("Icinga for Kubernetes has no current data for cluster %s.\nObject states are frozen until it reports again.", name)
		case now.Sub(seen) > t.policy.DaemonStaleAfter:
			desired, hold = SevCritical, t.policy.For
			reason = fmt.Sprintf("Icinga for Kubernetes has not reported for cluster %s for %s.\nObject states are frozen until it reports again.", name, now.Sub(seen).Round(time.Second))
		case here && !c.APIReachable:
			desired, hold = SevCritical, t.policy.For
			reason = fmt.Sprintf("Kubernetes API of cluster %s is unreachable from Icinga for Kubernetes.\n%s", name, c.DaemonMessage)
		default:
			// A cluster that is missing from the database (the daemon rebuilds its schema
			// on restart) is not an incident yet, but its objects are frozen meanwhile.
			trusted[name] = here
		}
		if ev, ok := t.step(key, "", desired, reason, now, hold); ok {
			events = append(events, ev)
		}
	}

	// 2. Objects of trusted clusters.
	seenKeys := map[Key]bool{}
	for _, o := range s.Objects {
		if !trusted[o.Key.Cluster] {
			continue
		}
		seenKeys[o.Key] = true
		if o.State == "pending" {
			// No verdict yet: keep whatever is currently reported and restart any timer.
			if r, ok := t.records[o.Key]; ok {
				r.candidateSet = false
				r.missingSince = time.Time{}
				r.uuid = o.UUID
			}
			continue
		}
		sev := t.policy.Severity[o.State]
		hold := t.policy.For
		if sev == SevNone {
			hold = t.policy.RecoverAfter
		}
		if ev, ok := t.step(o.Key, o.UUID, sev, o.Reason, now, hold); ok {
			events = append(events, ev)
		}
	}

	// 3. Objects that are no longer in the database.
	keys := make([]Key, 0, len(t.records))
	for k := range t.records {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		r := t.records[k]
		if k.Kind == KindCluster || seenKeys[k] {
			continue
		}
		if !trusted[k.Cluster] {
			// Frozen: timers must not run while the data is distrusted.
			r.missingSince = time.Time{}
			r.candidateSet = false
			continue
		}
		if r.reported == SevNone {
			delete(t.records, k) // was only a candidate
			continue
		}
		if r.missingSince.IsZero() {
			r.missingSince = now
		}
		if now.Sub(r.missingSince) < t.policy.GoneAfter {
			continue
		}
		if r.pendingID == "" || r.candidateSet {
			r.pendingID = t.newID()
			r.candidateSet = false
		}
		events = append(events, Event{
			ID: r.pendingID, Key: k, UUID: r.uuid, Severity: SevNone,
			Summary: k.title() + " no longer exists",
			Message: k.title() + " was removed from the cluster while it had an open problem.",
		})
	}
	return events
}

// step advances one object towards the desired severity and reports whether an event is due.
func (t *Tracker) step(k Key, uuid, desired, reason string, now time.Time, hold time.Duration) (Event, bool) {
	r, ok := t.records[k]
	if !ok {
		if desired == SevNone {
			return Event{}, false // healthy and not tracked: nothing to do
		}
		r = &record{}
		t.records[k] = r
	}
	r.missingSince = time.Time{}
	r.uuid = uuid

	if desired == r.reported {
		r.candidateSet = false
		r.pendingID = ""
		if desired == SevNone {
			delete(t.records, k)
		}
		return Event{}, false
	}
	if !r.candidateSet || r.candidate != desired {
		r.candidate, r.candidateSet, r.since = desired, true, now
		r.pendingID = ""
	}
	if now.Sub(r.since) < hold {
		return Event{}, false
	}
	if r.pendingID == "" {
		r.pendingID = t.newID()
	}
	summary, message := splitReason(reason)
	if summary == "" {
		word := desired
		if word == SevNone {
			word = "ok"
		}
		summary = k.title() + " is " + word
	}
	return Event{ID: r.pendingID, Key: k, UUID: uuid, Severity: desired, Summary: summary, Message: message}, true
}

// splitReason turns the daemon's multi-line reason into a one-line summary (at most 1024
// characters, the limit of Icinga Notifications) and the full message.
func splitReason(reason string) (summary, message string) {
	message = strings.TrimSpace(reason)
	summary = message
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = strings.TrimSpace(summary[:i])
	}
	if r := []rune(summary); len(r) > 1024 {
		summary = string(r[:1021]) + "..."
	}
	return summary, message
}
