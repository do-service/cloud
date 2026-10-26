package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNoDevice        = errors.New("auth: no such device")
	ErrInvalidPlatform = errors.New("auth: platform must be macos, windows or linux")
	ErrInvalidVersion  = errors.New("auth: client version must be at most 64 characters")
	ErrTooManyDevices  = errors.New("auth: too many devices")
)

const (
	// DeviceTokenPrefix marks a device token, so a leaked one is
	// recognizable (secret scanners, logs) and never mistaken for a
	// session or an app password.
	DeviceTokenPrefix = "cld_"
	deviceTokenBytes  = 32
	// MaxDevices per user: a person has a few computers; the cap keeps a
	// looping sign-in from filling the table.
	MaxDevices       = 20
	maxClientVersion = 64
	// deviceUsedEvery bounds how often last_used_at and last_ip are
	// written: a sync client calls the API all day long, and "seen today,
	// from there" is all the device list needs.
	deviceUsedEvery = time.Hour
)

var devicePlatforms = []string{"macos", "windows", "linux"}

// Device is a signed-in sync client; its token is shown only once, when
// it is created.
type Device struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform"`
	ClientVersion string     `json:"clientVersion"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUsedAt    *time.Time `json:"lastUsedAt"`
	LastIP        *string    `json:"lastIp"`
}

// Devices manages the bearer tokens of desktop sync clients.
type Devices struct {
	db *pgxpool.Pool
}

func NewDevices(db *pgxpool.Pool) *Devices { return &Devices{db: db} }

type deviceKey struct{}

// DeviceFromContext returns the device the request was authenticated
// with; false for a browser session.
func DeviceFromContext(ctx context.Context) (Device, bool) {
	d, ok := ctx.Value(deviceKey{}).(Device)
	return d, ok
}

func generateDeviceToken() string {
	var b [deviceTokenBytes]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	return DeviceTokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// deviceTokenLen is the length of every token generateDeviceToken makes.
var deviceTokenLen = len(DeviceTokenPrefix) + base64.RawURLEncoding.EncodedLen(deviceTokenBytes)

// Create registers a device of userID and returns it with its token, the
// one time the token is available in the clear.
func (d *Devices) Create(ctx context.Context, userID, name, platform, clientVersion string) (Device, string, error) {
	name, err := checkDevice(name, platform, clientVersion)
	if err != nil {
		return Device{}, "", err
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return Device{}, "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// As for app passwords: the user row serializes concurrent creates,
	// and the count, a statement of its own, sees what committed meanwhile.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return Device{}, "", fmt.Errorf("lock user: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM devices WHERE user_id = $1`, userID).Scan(&count); err != nil {
		return Device{}, "", fmt.Errorf("count devices: %w", err)
	}
	if count >= MaxDevices {
		return Device{}, "", ErrTooManyDevices
	}
	token := generateDeviceToken()
	var dev Device
	err = tx.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, platform, client_version, token_hash) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+deviceColumns,
		userID, name, platform, clientVersion, hashToken(token)).Scan(dev.fields()...)
	if err != nil {
		return Device{}, "", fmt.Errorf("create device: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Device{}, "", fmt.Errorf("commit: %w", err)
	}
	return dev, token, nil
}

// checkDevice validates what a client says about itself and returns the
// name as it is stored. The consent page's authorize checks the same
// before issuing a code, so a bad name fails there, not at the exchange.
func checkDevice(name, platform, clientVersion string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxAppName {
		return "", ErrInvalidName
	}
	if !slices.Contains(devicePlatforms, platform) {
		return "", ErrInvalidPlatform
	}
	if utf8.RuneCountInString(clientVersion) > maxClientVersion {
		return "", ErrInvalidVersion
	}
	return name, nil
}

const deviceColumns = `id, name, platform, client_version, created_at, last_used_at, host(last_ip)`

func (dev *Device) fields() []any {
	return []any{&dev.ID, &dev.Name, &dev.Platform, &dev.ClientVersion, &dev.CreatedAt, &dev.LastUsedAt, &dev.LastIP}
}

// List returns userID's devices, newest first.
func (d *Devices) List(ctx context.Context, userID string) ([]Device, error) {
	rows, err := d.db.Query(ctx, `SELECT `+deviceColumns+` FROM devices WHERE user_id = $1 ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Device, error) {
		var dev Device
		err := row.Scan(dev.fields()...)
		return dev, err
	})
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return list, nil
}

// Delete revokes one of userID's devices and returns what it was; someone
// else's is ErrNoDevice, as if it didn't exist. The token stops working
// with the next request: Lookup reads the table every time.
func (d *Devices) Delete(ctx context.Context, userID, id string) (Device, error) {
	var dev Device
	// Compared as text so a malformed id is simply not found.
	err := d.db.QueryRow(ctx, `DELETE FROM devices WHERE id::text = $1 AND user_id = $2 RETURNING `+deviceColumns,
		id, userID).Scan(dev.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNoDevice
	}
	if err != nil {
		return Device{}, fmt.Errorf("delete device: %w", err)
	}
	return dev, nil
}

// Lookup returns the user and device behind a token, the request coming
// from ip. An unknown token and a disabled account are both ErrNoDevice —
// checked on every request, so blocking someone signs out their clients
// at once.
func (d *Devices) Lookup(ctx context.Context, token string, ip netip.Addr) (User, Device, error) {
	if len(token) != deviceTokenLen || !strings.HasPrefix(token, DeviceTokenPrefix) {
		return User{}, Device{}, ErrNoDevice
	}
	var u User
	var dev Device
	err := d.db.QueryRow(ctx, `
		SELECT u.id, u.username, u.display_name, u.is_admin,
			d.id, d.name, d.platform, d.client_version, d.created_at, d.last_used_at, host(d.last_ip)
		FROM devices d JOIN users u ON u.id = d.user_id
		WHERE d.token_hash = $1 AND u.status = 'active'`,
		hashToken(token)).Scan(append([]any{&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin}, dev.fields()...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Device{}, ErrNoDevice
	}
	if err != nil {
		return User{}, Device{}, fmt.Errorf("look up device: %w", err)
	}
	if dev.LastUsedAt == nil || time.Since(*dev.LastUsedAt) >= deviceUsedEvery {
		var ipArg *netip.Addr
		if ip.IsValid() {
			ipArg = &ip
		}
		if _, err := d.db.Exec(ctx, `UPDATE devices SET last_used_at = now(), last_ip = $2 WHERE id = $1`, dev.ID, ipArg); err != nil {
			return User{}, Device{}, fmt.Errorf("touch device: %w", err)
		}
	}
	return u, dev, nil
}

// bearerToken is the credential of an "Authorization: Bearer …" header.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// deviceMayUse says whether a device token may make request r. A device
// is a sync client: it gets the files API, not what would let a stolen
// token outlive its revocation or reach further — administration, app
// passwords (a credential that survives revoking the device), and the
// management of devices, its own sign-out aside — nor sign in another
// device: consent to a new one takes a person at a browser, and a token
// that could mint tokens would survive its own revocation. Checked here, in
// Middleware, so a route added later is covered without its handler
// knowing devices exist.
func deviceMayUse(r *http.Request) bool {
	p := r.URL.Path
	if p == "/api/v1/me/devices/current" {
		return r.Method == http.MethodDelete
	}
	for _, denied := range []string{"/api/v1/admin", "/api/v1/me/app-passwords", "/api/v1/me/devices", "/api/v1/devices/authorize"} {
		if p == denied || strings.HasPrefix(p, denied+"/") {
			return false
		}
	}
	return true
}
