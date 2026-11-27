package mongodb

import (
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/mongo/options"
	"go.temporal.io/server/common/config"
)

const (
	defaultConnectTimeout         = 10 * time.Second
	defaultServerSelectionTimeout = 30 * time.Second
)

// buildClientOptions turns the MongoDB config into driver options. Hosts,
// replica set and credentials are set as options rather than in a URI, so
// user names and passwords need no escaping.
//
// No read preference and no write concern are set: reads use the driver
// default (primary), and writes and transactions use the write concern the
// MongoDB deployment defines (setDefaultRWConcern, or MongoDB's implicit
// default for the replica set). See ignoredConsistencySettings.
func buildClientOptions(cfg config.MongoDB) *options.ClientOptions {
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}
	// Server selection waits for a usable primary, for example during an
	// election, which can take longer than opening one connection. Never
	// wait less than the connect timeout.
	serverSelectionTimeout := defaultServerSelectionTimeout
	if connectTimeout > serverSelectionTimeout {
		serverSelectionTimeout = connectTimeout
	}

	opts := options.Client().
		SetHosts(cfg.Hosts).
		SetConnectTimeout(connectTimeout).
		SetServerSelectionTimeout(serverSelectionTimeout).
		SetHeartbeatInterval(10 * time.Second).
		SetRetryReads(true).
		SetRetryWrites(true)

	if cfg.ReplicaSet != "" {
		opts.SetReplicaSet(cfg.ReplicaSet)
	}
	if cfg.User != "" {
		opts.SetAuth(options.Credential{
			AuthSource:  cfg.AuthSource,
			Username:    cfg.User,
			Password:    cfg.Password,
			PasswordSet: cfg.Password != "",
		})
	}

	return opts
}

// ignoredConsistencySettings lists the readPreference and writeConcern
// values set in the config. They are not applied:
//   - Temporal needs to read its own writes. Reads outside transactions from a
//     lagging secondary could miss history or matching tasks, so every read
//     goes to the primary.
//   - The right write concern depends on the replica set (MongoDB uses w:1 on
//     a replica set with an arbiter, where majority would stop writes while a
//     data-bearing member is down). It is left to the deployment, where an
//     operator sets it once for every client with setDefaultRWConcern.
//
// The factory logs a warning for each value found, so that a setting that has
// no effect does not go unnoticed.
func ignoredConsistencySettings(cfg config.MongoDB) map[string]string {
	ignored := map[string]string{}
	if v := strings.TrimSpace(cfg.ReadPreference); v != "" {
		ignored["readPreference"] = v
	}
	if v := strings.TrimSpace(cfg.WriteConcern); v != "" {
		ignored["writeConcern"] = v
	}
	return ignored
}
