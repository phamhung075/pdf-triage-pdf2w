package httpapi

// Server is the HTTP server value a route-group hook receives. It aliases the package-private
// server type so a hook signature can name an exported type without changing that type's
// definition or the existing register* methods.
type Server = server

// routeGroupHooks collects the self-registration hooks contributed by route-group files. A route
// group lives in its own file and appends its registration method from an init function, so adding
// a route group never requires editing server.go:
//
//	func init() { routeGroupHooks = append(routeGroupHooks, (*Server).registerFoo) }
//
// The method takes only the server receiver and adds its routes to s.mux. NewServer runs every
// hook, in slice order, after the built-in register* calls and before it installs the handler.
var routeGroupHooks []func(*Server)
