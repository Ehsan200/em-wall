package main

import (
	"errors"
	"fmt"

	"github.com/ehsan/em-wall/core/ipc"
)

// errUnreachable marks a failure to connect to the socket at all, as
// opposed to the daemon answering with an error. It maps to a distinct
// exit code so scripts can tell "not installed / not running" apart
// from "the request was rejected".
var errUnreachable = errors.New("daemon not reachable")

// call opens a connection, makes one request, and closes it. The CLI is
// short-lived and single-shot, so it has no use for the app's pool.
func (a *app) call(method string, params, result any) error {
	c, err := ipc.Dial(a.socket)
	if err != nil {
		return fmt.Errorf("%w at %s (is em-walld running?): %v", errUnreachable, a.socket, err)
	}
	defer c.Close()
	return c.Call(method, params, result)
}
