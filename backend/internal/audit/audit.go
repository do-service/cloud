// Package audit keeps the audit log (R19): sign-ins, sharing, deletions
// and admins' actions, with who did them and from where. Recording never
// fails the request it describes: an entry that can't be written is
// logged instead.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Actions recorded. The admin's log filters by them.
const (
	Login           = "auth.login"
	LoginFailed     = "auth.login_failed"
	Logout          = "auth.logout"
	AppPasswordAdd  = "app_password.create"
	AppPasswordDrop = "app_password.delete"
	DeviceAuthorize = "device.authorize"
	DeviceRevoke    = "device.revoke"
	DeviceSignOut   = "device.signout"

	NodeTrash  = "node.trash"
	TrashPurge = "trash.purge"
	TrashEmpty = "trash.empty"

	ShareGrant  = "share.grant"
	ShareUpdate = "share.update"
	ShareRevoke = "share.revoke"
	LinkCreate  = "link.create"
	LinkUpdate  = "link.update"
	LinkDelete  = "link.delete"

	UserCreate   = "admin.user.create"
	UserUpdate   = "admin.user.update"
	UserDelete   = "admin.user.delete"
	GroupCreate  = "admin.group.create"
	GroupRename  = "admin.group.rename"
	GroupDelete  = "admin.group.delete"
	GroupMember  = "admin.group.member_add"
	GroupRemoved = "admin.group.member_remove"
)

// Actor is who did it: a user, or nobody known (a failed sign-in, a
// public link).
type Actor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Event is what happened.
type Event struct {
	Actor  Actor
	Action string
	// TargetID and TargetName say what it was done to: a node, a user, a
	// group. The name is kept as it was then.
	TargetID, TargetName string
	Details              map[string]any
}

// Entry is a recorded event.
type Entry struct {
	ID int64
	At time.Time
	Event
	IP string
}

type Log struct {
	db *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Log { return &Log{db: db} }

// Record writes e, done during r. A nil Log records nothing, for handlers
// set up without one (tests).
func (l *Log) Record(r *http.Request, e Event) {
	if l == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	var ip *string
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		s := ap.Addr().Unmap().String()
		ip = &s
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	var actor *string
	if e.Actor.ID != "" {
		actor = &e.Actor.ID
	}
	if _, err := l.db.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_name, ip, action, target_id, target_name, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		actor, e.Actor.Name, ip, e.Action, e.TargetID, e.TargetName, details); err != nil {
		slog.ErrorContext(ctx, "audit: record", "action", e.Action, "err", err)
	}
}

// Filter narrows List. Zero fields don't narrow.
type Filter struct {
	// Action is an action, or every action under a prefix ending in "."
	// ("admin.").
	Action string
	// Actor is a login, any case. It's matched by name, not id, so failed
	// sign-ins with unknown logins and deleted users' entries are found too.
	Actor string
	// From and To bound at: From inclusive, To exclusive.
	From, To time.Time
	// Query is part of the target's name, or its exact id.
	Query string
	// IP is an address or a network the entry came from.
	IP netip.Prefix
}

// likeEscaper makes user input literal inside a LIKE pattern (the default
// escape character is the backslash).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (f Filter) where() (string, []any) {
	var args []any
	where := "TRUE"
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Action != "" {
		if f.Action[len(f.Action)-1] == '.' {
			where += " AND starts_with(action, " + arg(f.Action) + ")"
		} else {
			where += " AND action = " + arg(f.Action)
		}
	}
	if f.Actor != "" {
		where += " AND lower(actor_name) = lower(" + arg(f.Actor) + ")"
	}
	if !f.From.IsZero() {
		where += " AND at >= " + arg(f.From)
	}
	if !f.To.IsZero() {
		where += " AND at < " + arg(f.To)
	}
	if f.Query != "" {
		n := arg(f.Query)
		where += " AND (target_name ILIKE '%' || " + arg(likeEscaper.Replace(f.Query)) + " || '%' OR target_id = " + n + ")"
	}
	if f.IP.IsValid() {
		where += " AND ip <<= " + arg(f.IP.String()) + "::inet"
	}
	return where, args
}

// List returns up to limit entries matching f, newest first, skipping the
// first offset of them, and how many match in all.
func (l *Log) List(ctx context.Context, f Filter, offset, limit int) (entries []Entry, total int64, err error) {
	where, args := f.where()
	if err := l.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit: count: %w", err)
	}
	args = append(args, limit, offset)
	rows, err := l.db.Query(ctx, fmt.Sprintf(`
		SELECT id, at, coalesce(actor_id::text, ''), actor_name, coalesce(host(ip), ''), action, target_id, target_name, details
		FROM audit_log WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list: %w", err)
	}
	entries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		var details []byte
		err := row.Scan(&e.ID, &e.At, &e.Actor.ID, &e.Actor.Name, &e.IP, &e.Action, &e.TargetID, &e.TargetName, &details)
		if err == nil {
			err = json.Unmarshal(details, &e.Details)
		}
		return e, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list: %w", err)
	}
	return entries, total, nil
}

// Prune deletes entries older than cutoff, at most limit at a time;
// it returns how many went.
func (l *Log) Prune(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	tag, err := l.db.Exec(ctx, `DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log WHERE at < $1 ORDER BY at LIMIT $2)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("audit: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}
