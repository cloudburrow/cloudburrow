// Package lifecycle coordinates startup ordering, readiness, and bounded
// shutdown. It owns every background worker in the process: services register
// work here rather than spawning detached goroutines. It also wires the narrow
// interfaces by which one service reaches another, so that services need not
// import each other.
package lifecycle
