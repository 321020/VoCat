package extensions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	ManifestFilename = "vocat-plugin.json"
	SchemaVersion    = 1
)

var pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

type Contribution struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	LabelZH  string `json:"label_zh,omitempty"`
	Location string `json:"location"`
	After    string `json:"after,omitempty"`
	Entry    string `json:"entry"`
}

type Backend struct {
	Commands map[string]string `json:"commands,omitempty"`
}

type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Version       string         `json:"version"`
	Description   string         `json:"description,omitempty"`
	Author        string         `json:"author,omitempty"`
	Homepage      string         `json:"homepage,omitempty"`
	Permissions   []string       `json:"permissions,omitempty"`
	Contributions []Contribution `json:"contributions"`
	Backend       *Backend       `json:"backend,omitempty"`
}

func DecodeManifest(reader io.Reader) (Manifest, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 256<<10))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", ManifestFilename, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("plugin manifest must contain one JSON object")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported plugin schema_version %d", manifest.SchemaVersion)
	}
	if !pluginIDPattern.MatchString(manifest.ID) {
		return errors.New("plugin id must be 3-64 lowercase letters, digits, or hyphens")
	}
	if strings.TrimSpace(manifest.Name) == "" || len(manifest.Name) > 100 {
		return errors.New("plugin name is required and must not exceed 100 characters")
	}
	if strings.TrimSpace(manifest.Version) == "" || len(manifest.Version) > 64 {
		return errors.New("plugin version is required and must not exceed 64 characters")
	}
	seen := make(map[string]struct{}, len(manifest.Contributions))
	for _, contribution := range manifest.Contributions {
		if !pluginIDPattern.MatchString(contribution.ID) {
			return fmt.Errorf("invalid contribution id %q", contribution.ID)
		}
		if _, duplicate := seen[contribution.ID]; duplicate {
			return fmt.Errorf("duplicate contribution id %q", contribution.ID)
		}
		seen[contribution.ID] = struct{}{}
		if contribution.Location != "sidebar" && contribution.Location != "proxy" {
			return fmt.Errorf("contribution %q has unsupported location %q", contribution.ID, contribution.Location)
		}
		if strings.TrimSpace(contribution.Label) == "" {
			return fmt.Errorf("contribution %q requires a label", contribution.ID)
		}
		if !safeRelativePath(contribution.Entry) {
			return fmt.Errorf("contribution %q has an unsafe entry path", contribution.ID)
		}
	}
	if manifest.Backend != nil {
		if len(manifest.Backend.Commands) == 0 {
			return errors.New("plugin backend commands are empty")
		}
		for platform, command := range manifest.Backend.Commands {
			if !strings.Contains(platform, "/") || !safeRelativePath(command) {
				return fmt.Errorf("plugin backend command for %q is invalid", platform)
			}
		}
	}
	permissions := append([]string(nil), manifest.Permissions...)
	sort.Strings(permissions)
	for index, permission := range permissions {
		if strings.TrimSpace(permission) == "" || (index > 0 && permission == permissions[index-1]) {
			return errors.New("plugin permissions must be non-empty and unique")
		}
	}
	return nil
}

func (manifest Manifest) BackendCommand() (string, bool) {
	if manifest.Backend == nil {
		return "", false
	}
	command, ok := manifest.Backend.Commands[runtime.GOOS+"/"+runtime.GOARCH]
	return command, ok
}

func safeRelativePath(value string) bool {
	value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, ":") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
