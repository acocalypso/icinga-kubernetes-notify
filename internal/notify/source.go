package notify

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Reader loads snapshots from the Icinga for Kubernetes database. It only runs SELECTs.
type Reader struct {
	db    *sql.DB
	watch WatchConfig
}

func OpenReader(c DatabaseConfig, w WatchConfig) (*Reader, error) {
	mc := mysql.NewConfig()
	mc.Net = "tcp"
	mc.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	mc.User = c.User
	mc.Passwd = c.Password
	mc.DBName = c.Database
	mc.Timeout = 10 * time.Second
	mc.ReadTimeout = 30 * time.Second
	if c.TLS {
		mc.TLSConfig = "true"
	}
	conn, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)
	return &Reader{db: db, watch: w}, nil
}

func (r *Reader) Close() error { return r.db.Close() }

// Snapshot reads cluster health and the state of every watched object.
func (r *Reader) Snapshot(ctx context.Context) (Snapshot, error) {
	// One timestamp from the database for the heartbeat age, so clock differences between
	// this process and the database host do not matter.
	s := Snapshot{Time: time.Now()}
	clusterNames := map[string]string{} // hex(uuid) -> name

	rows, err := r.db.QueryContext(ctx, `
		SELECT c.uuid, c.name,
		       CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS SIGNED) - COALESCE(MAX(i.heartbeat), 0),
		       COALESCE(MAX(i.kubernetes_api_reachable = 'y'), 0),
		       COALESCE(MAX(i.message), '')
		FROM cluster c LEFT JOIN kubernetes_instance i ON i.cluster_uuid = c.uuid
		GROUP BY c.uuid, c.name`)
	if err != nil {
		return s, fmt.Errorf("read clusters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			uuid      []byte
			name, msg string
			ageMs     int64
			reachable bool
		)
		if err := rows.Scan(&uuid, &name, &ageMs, &reachable, &msg); err != nil {
			return s, fmt.Errorf("read clusters: %w", err)
		}
		if !r.clusterWanted(name) {
			continue
		}
		clusterNames[hex.EncodeToString(uuid)] = name
		s.Clusters = append(s.Clusters, Cluster{
			Name: name, HeartbeatAge: time.Duration(ageMs) * time.Millisecond,
			APIReachable: reachable, DaemonMessage: msg,
		})
	}
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("read clusters: %w", err)
	}

	for _, kind := range r.watch.Kinds {
		info := kinds[kind]
		// Table names come from the fixed map above, never from user input.
		q := fmt.Sprintf("SELECT cluster_uuid, uuid, COALESCE(namespace, ''), name, icinga_state, COALESCE(icinga_state_reason, '') FROM `%s`", info.table)
		if err := r.readKind(ctx, q, kind, info, clusterNames, &s); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (r *Reader) readKind(ctx context.Context, q, kind string, info kindInfo, clusters map[string]string, s *Snapshot) error {
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read %s: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cluster, uuid           []byte
			ns, name, state, reason string
		)
		if err := rows.Scan(&cluster, &uuid, &ns, &name, &state, &reason); err != nil {
			return fmt.Errorf("read %s: %w", kind, err)
		}
		clusterName, ok := clusters[hex.EncodeToString(cluster)]
		if !ok {
			continue
		}
		if !info.namespaced {
			ns = ""
		} else if !r.namespaceWanted(ns) {
			continue
		}
		s.Objects = append(s.Objects, Object{
			Key:    Key{Cluster: clusterName, Kind: kind, Namespace: ns, Name: name},
			UUID:   formatUUID(uuid),
			State:  state,
			Reason: reason,
		})
	}
	return rows.Err()
}

func (r *Reader) clusterWanted(name string) bool {
	if len(r.watch.Clusters) == 0 {
		return true
	}
	for _, c := range r.watch.Clusters {
		if c == name {
			return true
		}
	}
	return false
}

func (r *Reader) namespaceWanted(ns string) bool {
	return namespaceWanted(ns, r.watch.Namespaces, r.watch.ExcludeNamespaces)
}

func namespaceWanted(ns string, include, exclude []string) bool {
	for _, p := range exclude {
		if ok, _ := path.Match(p, ns); ok {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, p := range include {
		if ok, _ := path.Match(p, ns); ok {
			return true
		}
	}
	return false
}

func formatUUID(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
