package port_test

import (
	"context"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// Verify the Out ports are interfaces with the expected method shapes by
// constructing tiny anonymous implementations. If a method signature drifts,
// these will fail to compile — the test's purpose.
func TestOutPortsAreImplementable(t *testing.T) {
	t.Parallel()

	var _ port.GeoSource = stubGeoSource{}
	var _ port.Cache = stubCache{}
	var _ port.Clock = stubClock{}
	var _ port.ConfigLoader = stubConfigLoader{}
}

type stubGeoSource struct{}

func (stubGeoSource) Fetch(_ context.Context, _ string) ([]byte, error) { return nil, nil }

type stubCache struct{}

func (stubCache) Read(_ string) (port.RURanges, error)              { return port.RURanges{}, nil }
func (stubCache) Write(_ string, _ port.RURanges) error             { return nil }
func (stubCache) Freshness(_ string, _ time.Duration, _ time.Time) (bool, error) {
	return false, nil
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Unix(0, 0) }

type stubConfigLoader struct{}

func (stubConfigLoader) Load(_ string) (port.Config, error) { return port.Config{}, nil }
func (stubConfigLoader) Validate(_ port.Config) error       { return nil }
