package templates

// StaticVersion fingerprints the embedded stylesheet; set at init by the ui
// package (the embed lives there). The layout appends it as ?v= so the
// day-long browser cache can never pair last release's CSS with this
// release's markup - which looked exactly like a layout bug.
var StaticVersion = "dev"
