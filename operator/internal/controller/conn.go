/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"io"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// closeConn closes a connection to slapd the caller has finished with. A
// failed close cannot undo what the connection already did, so it is logged
// rather than returned: failing a reconcile, or reading a reachable peer as
// unreachable, over it would misreport work that succeeded.
func closeConn(ctx context.Context, c io.Closer) {
	if err := c.Close(); err != nil {
		logf.FromContext(ctx).V(1).Info("closing connection failed", "err", err)
	}
}
