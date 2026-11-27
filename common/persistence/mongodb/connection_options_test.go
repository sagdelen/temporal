package mongodb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/server/common/config"
)

func TestBuildClientOptions(t *testing.T) {
	cfg := config.MongoDB{
		Hosts:          []string{"a:27017", "b:27017"},
		User:           "temporal",
		Password:       "p@ss:w/rd",
		AuthSource:     "admin",
		ReplicaSet:     "rs0",
		WriteConcern:   "w1",
		ReadPreference: "nearest",
	}
	opts := buildClientOptions(cfg)
	require.NoError(t, opts.Validate())

	require.Equal(t, cfg.Hosts, opts.Hosts)
	require.Equal(t, "rs0", *opts.ReplicaSet)
	require.Equal(t, "temporal", opts.Auth.Username)
	require.Equal(t, "p@ss:w/rd", opts.Auth.Password)
	require.Equal(t, "admin", opts.Auth.AuthSource)
	require.Equal(t, defaultConnectTimeout, *opts.ConnectTimeout)
	require.Equal(t, defaultServerSelectionTimeout, *opts.ServerSelectionTimeout)

	// Neither setting reaches the driver: reads use the primary and writes the
	// deployment's write concern.
	require.Nil(t, opts.WriteConcern)
	require.Nil(t, opts.ReadPreference)

	cfg.ConnectTimeout = time.Minute
	opts = buildClientOptions(cfg)
	require.Equal(t, time.Minute, *opts.ConnectTimeout)
	require.Equal(t, time.Minute, *opts.ServerSelectionTimeout)

	cfg.User = ""
	require.Nil(t, buildClientOptions(cfg).Auth)
}

func TestIgnoredConsistencySettings(t *testing.T) {
	require.Empty(t, ignoredConsistencySettings(config.MongoDB{}))
	require.Empty(t, ignoredConsistencySettings(config.MongoDB{ReadPreference: " ", WriteConcern: ""}))
	require.Equal(t,
		map[string]string{"readPreference": "secondary", "writeConcern": "majority"},
		ignoredConsistencySettings(config.MongoDB{ReadPreference: "secondary", WriteConcern: "majority"}),
	)
}
