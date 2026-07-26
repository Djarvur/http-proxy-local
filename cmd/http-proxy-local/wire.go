package main

// wire.go is the composition root for the http-proxy-local binary.
//
// As the Out ports (ripe, fscache, yamlconfig, fsnotify), the In adapter
// (internal/in/http), and the app layer (internal/app) land in subsequent
// task groups, this file will grow to construct them, hand them to a Runtime,
// and wire the hot-reload + PAC-regeneration signal paths.
//
// It is intentionally separate from main.go so that main.go stays a thin
// flags+signal shell and the dependency graph reads in one place here.
