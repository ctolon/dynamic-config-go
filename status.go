package dynamicconfig

import "time"

// Status is a snapshot of the configuration machinery's health.
//
// It contains counters, timestamps and state — never configuration values.
// That is deliberate: Status is meant to be safe to expose from a /healthz
// or /debug endpoint and to hand straight to a metrics exporter, and a
// struct that carried the decoded configuration could not be either.
//
// A typical export:
//
//	dynamic_config_generation              Status.Generation
//	dynamic_config_reload_success_total    Status.SuccessfulReloads
//	dynamic_config_reload_failure_total    Status.FailedReloads
//	dynamic_config_events_dropped_total    Status.DroppedEvents
//	dynamic_config_last_success_timestamp  Status.LastSuccess
type Status struct {
	// Generation is the number of snapshots published, counting the
	// initial load. It only ever increases, and it increases only on a
	// successful publication, so a stuck generation next to a rising
	// FailedReloads is the signature of a configuration file that no
	// longer validates.
	Generation uint64

	// SuccessfulReloads counts reloads that published a snapshot, not
	// counting the initial load. Generation equals SuccessfulReloads + 1.
	SuccessfulReloads uint64

	// FailedReloads counts reload attempts rejected at the read, decode or
	// validation stage. Each one left the published snapshot untouched.
	FailedReloads uint64

	// LastSuccess is when the most recent snapshot was published; for a
	// configuration that has never reloaded, the time of the initial load.
	LastSuccess time.Time

	// LastFailure is when the most recent reload was rejected. Zero if
	// none has been.
	LastFailure time.Time

	// Watching reports whether a watch is established — the directory is
	// armed and changes are being observed. It is deliberately not "a
	// call to Watch is in progress": between claiming the watcher slot
	// and arming the watch there is a moment during which a change would
	// be caught by the startup check rather than by an event, and
	// reporting true there would promise something not yet true.
	Watching bool

	// Closed reports whether Close has been called.
	Closed bool

	// ConfigFile is the primary configuration file — the first one
	// layered — or empty for a configuration that has none.
	ConfigFile string

	// ConfigFiles is every file the published snapshot was read from, in
	// the order they were layered, later files having overridden earlier
	// ones. It has one element for the ordinary single-file case, and
	// none for a configuration built from defaults and the environment.
	ConfigFiles []string

	// DroppedEvents counts subscriber notifications discarded because the
	// bounded dispatch queue was full — that is, because a subscriber
	// could not keep up. Publication is never affected by this; see the
	// delivery contract in doc.go. A rising count means a slow
	// subscriber, and subscribers that need authoritative state should
	// read Current rather than rely on delivery.
	DroppedEvents uint64
}
