package update

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Mode is the user's «Обновления» setting.
type Mode string

const (
	// ModeOff: no checks at all («Не проверять»).
	ModeOff Mode = "off"
	// ModeNotify: check and tell; download when the user asks («Сообщать
	// о новой версии»).
	ModeNotify Mode = "notify"
	// ModeAuto: check, download and verify in the background, then ask
	// before installing («Скачивать автоматически, спрашивать перед
	// установкой»). The default.
	ModeAuto Mode = "auto"
)

// DefaultMode is the mode until the user picks one.
const DefaultMode = ModeAuto

// Valid reports whether m is one of the modes.
func (m Mode) Valid() bool { return m == ModeOff || m == ModeNotify || m == ModeAuto }

// SettingsFile is the app's settings in the state directory
// (state.BaseDir()): a JSON object, of which this package owns the
// "updates" key. Other keys are kept as they are, for the settings that
// come next (E14-5).
const SettingsFile = "settings.json"

const modeKey = "updates"

// LoadMode reads the mode from base's settings file: DefaultMode when the
// file or the key is missing, or the value unknown (a newer client's).
func LoadMode(base string) (Mode, error) {
	all, err := readSettings(base)
	if err != nil {
		return DefaultMode, err
	}
	var m Mode
	if raw, ok := all[modeKey]; ok && json.Unmarshal(raw, &m) == nil && m.Valid() {
		return m, nil
	}
	return DefaultMode, nil
}

// SaveMode writes the mode into base's settings file, replacing it
// atomically (a crash leaves the old file or the new one).
func SaveMode(base string, m Mode) error {
	if !m.Valid() {
		return errors.New("update: unknown mode " + string(m))
	}
	all, err := readSettings(base)
	if err != nil {
		// A broken file isn't worth keeping over the user's choice.
		all = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	all[modeKey] = raw
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(base, SettingsFile+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(base, SettingsFile))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func readSettings(base string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(filepath.Join(base, SettingsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	return all, nil
}
