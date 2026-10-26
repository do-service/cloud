// Package share manages who else may use a node (R9): shares to users and
// groups, the groups themselves (R15), and the directory a sharer picks
// recipients from. What a share allows is decided by internal/access; this
// package only records the grants.
package share

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/fs"
)

var (
	// ErrInvalidGrantee: no such active user or group, or the node's owner.
	ErrInvalidGrantee = errors.New("share: no such user or group to share with")
	// ErrInvalidPerms: no read, unknown bits, or more than the sharer has.
	ErrInvalidPerms  = errors.New("share: rights must include read and not exceed the sharer's")
	ErrNoShare       = errors.New("share: no such share")
	ErrNoGroup       = errors.New("share: no such group")
	ErrGroupName     = errors.New("share: group name must be 1 to 100 characters")
	ErrGroupExists   = errors.New("share: a group with this name exists")
	ErrNotRecipients = errors.New("share: only users and groups can be shared with")
)

const (
	GranteeUser  = "user"
	GranteeGroup = "group"
)

// Grantee is who a share is for.
type Grantee struct {
	Type string
	ID   string
	// Name is a user's display name (else username) or a group's name.
	Name string
}

type Share struct {
	ID        string
	NodeID    string
	Grantee   Grantee
	Perms     access.Perm
	CreatedAt time.Time
}

type Service struct {
	db    *pgxpool.Pool
	files *fs.Service
}

func New(db *pgxpool.Pool, files *fs.Service) *Service { return &Service{db: db, files: files} }

// sharable returns the node the actor may manage shares of: theirs, or
// one they may reshare.
func (s *Service) sharable(ctx context.Context, actorID, nodeID string) (fs.Node, error) {
	n, err := s.files.Get(ctx, actorID, nodeID)
	if err != nil {
		return fs.Node{}, err
	}
	if !n.Perms.Has(access.Reshare) {
		return fs.Node{}, fs.ErrForbidden
	}
	return n, nil
}

const shareColumns = `s.id::text, s.node_id::text, s.grantee_type, s.grantee_id::text,
	coalesce(nullif(u.display_name, ''), u.username::text, g.name::text, ''), s.perms, s.created_at`

const shareJoins = `LEFT JOIN users u ON s.grantee_type = 'user' AND u.id = s.grantee_id
	LEFT JOIN groups g ON s.grantee_type = 'group' AND g.id = s.grantee_id`

func scanShare(row pgx.Row) (Share, error) {
	var sh Share
	var perms int16
	err := row.Scan(&sh.ID, &sh.NodeID, &sh.Grantee.Type, &sh.Grantee.ID, &sh.Grantee.Name, &perms, &sh.CreatedAt)
	sh.Perms = access.Perm(perms)
	if errors.Is(err, pgx.ErrNoRows) {
		return Share{}, ErrNoShare
	}
	return sh, err
}

// List returns the shares made on the node itself (not those it inherits),
// to its owner or someone who may reshare it.
func (s *Service) List(ctx context.Context, actorID, nodeID string) ([]Share, error) {
	n, err := s.sharable(ctx, actorID, nodeID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+shareColumns+` FROM shares s `+shareJoins+`
		WHERE s.node_id = $1 ORDER BY s.created_at, s.id`, n.ID)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Share, error) { return scanShare(row) })
}

// Grant shares the node with a user or a group, or changes the rights of
// an existing share to them. No one shares more than they have.
func (s *Service) Grant(ctx context.Context, actorID, nodeID, granteeType, granteeID string, perms access.Perm) (Share, error) {
	n, err := s.sharable(ctx, actorID, nodeID)
	if err != nil {
		return Share{}, err
	}
	if !validPerms(perms, n.Perms) {
		return Share{}, ErrInvalidPerms
	}
	if err := s.checkGrantee(ctx, granteeType, granteeID, n.OwnerID); err != nil {
		return Share{}, err
	}
	// Changing an existing share is held to the same bound as the new
	// rights: a resharer can't cut down a share wider than their own.
	var existing int16
	err = s.db.QueryRow(ctx, `SELECT perms FROM shares WHERE node_id = $1 AND grantee_type = $2 AND grantee_id = $3`, n.ID, granteeType, granteeID).Scan(&existing)
	switch {
	case err == nil && !n.Perms.Has(access.Perm(existing)):
		return Share{}, fs.ErrForbidden
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Share{}, err
	}
	var sh Share
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH upserted AS (
				INSERT INTO shares (node_id, grantee_type, grantee_id, perms, created_by) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (node_id, grantee_type, grantee_id) DO UPDATE SET perms = EXCLUDED.perms
				RETURNING *
			)
			SELECT `+shareColumns+` FROM upserted s `+shareJoins, n.ID, granteeType, granteeID, int16(perms), actorID)
		if sh, err = scanShare(row); err != nil {
			return err
		}
		return touchAccess(ctx, tx, sh.Grantee)
	})
	return sh, err
}

// Update changes a share's rights, under the same rules as Grant.
func (s *Service) Update(ctx context.Context, actorID, shareID string, perms access.Perm) (Share, error) {
	sh, err := s.get(ctx, shareID)
	if err != nil {
		return Share{}, err
	}
	n, err := s.sharable(ctx, actorID, sh.NodeID)
	if err != nil {
		return Share{}, notVisible(err)
	}
	if !validPerms(perms, n.Perms) {
		return Share{}, ErrInvalidPerms
	}
	if !n.Perms.Has(sh.Perms) {
		return Share{}, fs.ErrForbidden
	}
	var updated Share
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if updated, err = scanShare(tx.QueryRow(ctx, `
			WITH updated AS (UPDATE shares SET perms = $2 WHERE id = $1 RETURNING *)
			SELECT `+shareColumns+` FROM updated s `+shareJoins, sh.ID, int16(perms))); err != nil {
			return err
		}
		return touchAccess(ctx, tx, updated.Grantee)
	})
	return updated, err
}

// Revoke deletes a share: as the node's owner, as a resharer whose own
// rights cover the share's, or as the user it is for, leaving a share they
// no longer want. It applies at once.
func (s *Service) Revoke(ctx context.Context, actorID, shareID string) error {
	sh, err := s.get(ctx, shareID)
	if err != nil {
		return err
	}
	leaving := sh.Grantee.Type == GranteeUser && sh.Grantee.ID == actorID
	if !leaving {
		n, err := s.sharable(ctx, actorID, sh.NodeID)
		if err != nil {
			return notVisible(err)
		}
		// No one revokes more than they could have granted.
		if !n.Perms.Has(sh.Perms) {
			return fs.ErrForbidden
		}
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM shares WHERE id = $1`, sh.ID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return touchAccess(ctx, tx, sh.Grantee)
	})
}

// touchAccess tells the sync clients of whoever a share is for that what
// they see changed (fs.TouchAccess), in the transaction that changed it.
func touchAccess(ctx context.Context, tx pgx.Tx, g Grantee) error {
	if g.Type == GranteeGroup {
		return fs.TouchAccess(ctx, tx, nil, []string{g.ID})
	}
	return fs.TouchAccess(ctx, tx, []string{g.ID}, nil)
}

func (s *Service) get(ctx context.Context, shareID string) (Share, error) {
	if !validUUID(shareID) {
		return Share{}, ErrNoShare
	}
	return scanShare(s.db.QueryRow(ctx, `SELECT `+shareColumns+` FROM shares s `+shareJoins+` WHERE s.id = $1`, shareID))
}

// notVisible hides a share on a node the actor can't see at all.
func notVisible(err error) error {
	if errors.Is(err, fs.ErrNotFound) {
		return ErrNoShare
	}
	return err
}

func validPerms(p, sharer access.Perm) bool {
	return p.Has(access.Read) && p&^access.All == 0 && sharer.Has(p)
}

func (s *Service) checkGrantee(ctx context.Context, granteeType, granteeID, ownerID string) error {
	if !validUUID(granteeID) {
		return ErrInvalidGrantee
	}
	var exists bool
	var err error
	switch granteeType {
	case GranteeUser:
		if granteeID == ownerID {
			return ErrInvalidGrantee
		}
		err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND status = 'active')`, granteeID).Scan(&exists)
	case GranteeGroup:
		err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM groups WHERE id = $1)`, granteeID).Scan(&exists)
	default:
		return ErrNotRecipients
	}
	if err != nil {
		return err
	}
	if !exists {
		return ErrInvalidGrantee
	}
	return nil
}

// SharedItem is a node someone shared with the actor.
type SharedItem struct {
	fs.Node
	Owner    Grantee
	SharedAt time.Time
}

// SharedWithMe returns what others shared with the actor or their groups,
// each the topmost shared folder only: one shared inside another already
// shared with them is reached through the outer one.
func (s *Service) SharedWithMe(ctx context.Context, actorID string) ([]SharedItem, error) {
	rows, err := s.db.Query(ctx, `
		WITH mine AS (
			SELECT s.node_id, s.created_at FROM shares s
			WHERE (s.grantee_type = 'user' AND s.grantee_id = $1)
			   OR (s.grantee_type = 'group' AND s.grantee_id IN (SELECT group_id FROM group_members WHERE user_id = $1))
		)
		SELECT n.id::text, min(m.created_at), o.id::text, coalesce(nullif(o.display_name, ''), o.username::text)
		FROM mine m JOIN nodes n ON n.id = m.node_id JOIN users o ON o.id = n.owner_id
		WHERE n.trashed_at IS NULL AND n.owner_id <> $1
		  AND NOT EXISTS (
			WITH RECURSIVE up AS (
				SELECT parent_id FROM nodes WHERE id = n.id
				UNION ALL
				SELECT p.parent_id FROM nodes p JOIN up ON p.id = up.parent_id
			)
			SELECT 1 FROM up JOIN mine m2 ON m2.node_id = up.parent_id)
		GROUP BY n.id, o.id
		ORDER BY min(m.created_at) DESC`, actorID)
	if err != nil {
		return nil, fmt.Errorf("shared with me: %w", err)
	}
	type ref struct {
		id    string
		at    time.Time
		owner Grantee
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ref, error) {
		r := ref{owner: Grantee{Type: GranteeUser}}
		err := row.Scan(&r.id, &r.at, &r.owner.ID, &r.owner.Name)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("shared with me: %w", err)
	}
	items := make([]SharedItem, 0, len(refs))
	for _, r := range refs {
		// Through fs, so the node and its rights are what any other
		// request would see.
		n, err := s.files.Get(ctx, actorID, r.id)
		if errors.Is(err, fs.ErrNotFound) {
			continue // revoked or deleted meanwhile
		}
		if err != nil {
			return nil, err
		}
		items = append(items, SharedItem{Node: n, Owner: r.owner, SharedAt: r.at})
	}
	return items, nil
}

// Entry is a shared item under the name the recipient's list of shares
// shows it by: WebDAV's «Доступно мне», a sync client's roots.
type Entry struct {
	Item SharedItem
	Name string
}

// Entries names items (SharedWithMe's) for a list of them, in the order
// they were shared. Names repeat across owners: those are told apart by
// the owner's name, then by a number, which keeps each entry's name
// stable as more are shared.
func Entries(items []SharedItem) []Entry {
	items = slices.Clone(items)
	slices.SortStableFunc(items, func(a, b SharedItem) int {
		return cmp.Or(a.SharedAt.Compare(b.SharedAt), strings.Compare(a.ID, b.ID))
	})
	count := map[string]int{}
	for _, it := range items {
		count[nameKey(it.Name)]++
	}
	taken := map[string]bool{}
	entries := make([]Entry, 0, len(items))
	for _, it := range items {
		name := it.Name
		if count[nameKey(name)] > 1 {
			name = fmt.Sprintf("%s (%s)", it.Name, it.Owner.Name)
		}
		base := name
		for i := 2; taken[nameKey(name)]; i++ {
			name = fmt.Sprintf("%s (%d)", base, i)
		}
		taken[nameKey(name)] = true
		entries = append(entries, Entry{Item: it, Name: name})
	}
	return entries
}

// nameKey is how the tree compares names: two that match can't sit in
// one folder.
func nameKey(name string) string {
	if _, key, err := fs.NormalizeName(name); err == nil {
		return key
	}
	return name
}

// Directory finds users (but the actor) and groups to share with, by a
// part of their name; at most limit of each.
func (s *Service) Directory(ctx context.Context, actorID, query string, limit int) ([]Grantee, error) {
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(query)) + "%"
	rows, err := s.db.Query(ctx, `
		(SELECT 'user', id::text, coalesce(nullif(display_name, ''), username::text) FROM users
		 WHERE status = 'active' AND id <> $1 AND (username ILIKE $2 OR display_name ILIKE $2)
		 ORDER BY username LIMIT $3)
		UNION ALL
		(SELECT 'group', id::text, name::text FROM groups WHERE name ILIKE $2 ORDER BY name LIMIT $3)`,
		actorID, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Grantee])
}

// Group is a named set of users.
type Group struct {
	ID      string
	Name    string
	Members []Grantee
}

func normalizeGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", ErrGroupName
	}
	return name, nil
}

func mapGroupConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrGroupExists
	}
	return err
}

// Groups lists the groups with their members. Admin only (the caller
// checks).
func (s *Service) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id::text, g.name::text, u.id::text, coalesce(nullif(u.display_name, ''), u.username::text)
		FROM groups g LEFT JOIN group_members m ON m.group_id = g.id LEFT JOIN users u ON u.id = m.user_id
		ORDER BY g.name, u.username`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	var groups []Group
	var id, name string
	var memberID, memberName *string
	_, err = pgx.ForEachRow(rows, []any{&id, &name, &memberID, &memberName}, func() error {
		if len(groups) == 0 || groups[len(groups)-1].ID != id {
			groups = append(groups, Group{ID: id, Name: name, Members: []Grantee{}})
		}
		if memberID != nil {
			g := &groups[len(groups)-1]
			g.Members = append(g.Members, Grantee{Type: GranteeUser, ID: *memberID, Name: *memberName})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	return groups, nil
}

func (s *Service) CreateGroup(ctx context.Context, name string) (Group, error) {
	name, err := normalizeGroupName(name)
	if err != nil {
		return Group{}, err
	}
	g := Group{Name: name, Members: []Grantee{}}
	err = s.db.QueryRow(ctx, `INSERT INTO groups (name) VALUES ($1) RETURNING id::text`, name).Scan(&g.ID)
	return g, mapGroupConflict(err)
}

func (s *Service) RenameGroup(ctx context.Context, id, name string) error {
	name, err := normalizeGroupName(name)
	if err != nil {
		return err
	}
	if !validUUID(id) {
		return ErrNoGroup
	}
	tag, err := s.db.Exec(ctx, `UPDATE groups SET name = $2 WHERE id = $1`, id, name)
	if err != nil {
		return mapGroupConflict(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoGroup
	}
	return nil
}

// DeleteGroup deletes a group and every share made to it: its members
// lose that access at once.
func (s *Service) DeleteGroup(ctx context.Context, id string) error {
	if !validUUID(id) {
		return ErrNoGroup
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The members, read before the cascade takes them: whose view changes.
	rows, err := tx.Query(ctx, `SELECT user_id::text FROM group_members WHERE group_id = $1`, id)
	if err != nil {
		return err
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM shares WHERE grantee_type = 'group' AND grantee_id = $1`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoGroup
	}
	if err := fs.TouchAccess(ctx, tx, members, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetMember adds userID to the group, or removes them.
func (s *Service) SetMember(ctx context.Context, groupID, userID string, member bool) error {
	if !validUUID(groupID) {
		return ErrNoGroup
	}
	if !validUUID(userID) {
		return ErrInvalidGrantee
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		if member {
			tag, err = tx.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, groupID, userID)
		} else {
			tag, err = tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
		}
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		// They gain or lose what is shared with the group.
		return fs.TouchAccess(ctx, tx, []string{userID}, nil)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		if pgErr.ConstraintName == "group_members_group_id_fkey" {
			return ErrNoGroup
		}
		return ErrInvalidGrantee
	}
	return err
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'):
			return false
		}
	}
	return true
}
