// This Source Code Form is subject to the terms of the Apache License, Version 2.0.
// If a copy of the  APLV2 was not distributed with this file,
// You can obtain one at http://www.apache.org/licenses/LICENSE-2.0.

// Adapted from https://github.com/helm/helm/blob/v3.8.0/pkg/registry/utils.go

package registry

import (
	"bytes"
	"context"
	"io"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	orascontext "oras.land/oras-go/pkg/context"
)

// ctx retrieves a fresh context.
// disable verbose logging coming from ORAS (unless debug is enabled)
func ctx(out io.Writer, debug bool) context.Context {
	if !debug {
		return orascontext.Background()
	}
	ctx := orascontext.WithLoggerFromWriter(context.Background(), out)
	orascontext.GetLogger(ctx).Logger.SetLevel(logrus.DebugLevel)
	return ctx
}

// extractChartMeta is used to extract a chart metadata from a byte array
func extractChartMeta(chartData []byte) (*chart.Metadata, error) {
	ch, err := loader.LoadArchive(bytes.NewReader(chartData))
	if err != nil {
		return nil, err
	}
	return ch.Metadata, nil
}
