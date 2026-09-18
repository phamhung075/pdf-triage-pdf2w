package httpapi

// Hub returns the server-owned SSE fan-out. GET /api/triage/events subscribes to it, and every
// SCAN_*/FILE_*/TASK_* event a mutation route broadcasts goes through it. Exposing it lets the
// composition root's auto-watcher share the same hub (WatcherDeps.Hub) instead of broadcasting to a
// private, subscriber-less hub, so an AUTO-scan's raw SCAN_STARTED / FILE_PROGRESS / FILE_COMPLETED
// / SCAN_COMPLETED frames reach dashboard clients exactly like a manual scan's do.
//
// This is the smallest fix for the interface gap reported in cmd/pdf-triage/doc.go: NewServer keeps
// returning http.Handler, every existing exported signature is untouched, and app.go already
// captures the *Server through a RouteGroup, so one accessor is enough. A Deps-free constructor (or
// a Hub field on Deps) would either duplicate NewServer's construction path or widen the public Deps
// surface for no additional reach.
func (s *Server) Hub() *Hub {
	return s.hub
}
