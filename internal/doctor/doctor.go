// Package doctor inspects fixed utility versions and configured filesystems.
// It never discovers, probes, moves, opens or configures a tape device.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/maxya/TapePilot/internal/config"
	"golang.org/x/sys/unix"
)

type Check struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Message        string `json:"message"`
	Version        string `json:"version,omitempty"`
	AvailableBytes *int64 `json:"available_bytes,omitempty"`
	RequiredBytes  *int64 `json:"required_bytes,omitempty"`
	Identity       string `json:"identity,omitempty"`
}
type Report struct {
	Mode                   string  `json:"mode"`
	CheckedAt              string  `json:"checked_at"`
	Ready                  bool    `json:"ready"`
	PhysicalReadConfigured bool    `json:"physical_read_configured"`
	Checks                 []Check `json:"checks"`
}

// Probe is replaceable for tests. Production always receives a fixed version
// invocation from the package allowlist, independent of HTTP/configuration input.
type Probe func(context.Context, string, []string) (string, error)
type Inspector struct{ Probe Probe }

type utility struct {
	name, path string
	args       []string
	required   bool
}

var tools = []utility{
	{"tar", "/usr/bin/tar", []string{"--version"}, true},
	{"mbuffer", "/usr/bin/mbuffer", []string{"--version"}, false},
	{"mt-st", "/usr/bin/mt", []string{"--version"}, false},
	{"lsscsi", "/usr/bin/lsscsi", []string{"--version"}, false},
}
var versionNumber = regexp.MustCompile(`\b(?:[0-9]{8}|[0-9]+(?:\.[0-9]+){1,3}(?:[a-z][0-9]*)?)\b`)

func Inspect(ctx context.Context, c config.Settings) Report { return (Inspector{}).Inspect(ctx, c) }
func (i Inspector) Inspect(ctx context.Context, c config.Settings) Report {
	report := Report{Mode: "simulator", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Ready: true, PhysicalReadConfigured: c.PhysicalRead != nil, Checks: []Check{}}
	add := func(check Check) {
		report.Checks = append(report.Checks, check)
		if check.Status == "error" {
			report.Ready = false
		}
	}
	probe := i.Probe
	if probe == nil {
		probe = version
	}
	for _, tool := range tools {
		if err := ctx.Err(); err != nil {
			add(Check{ID: "inspection", Status: "error", Message: "Inspection was cancelled before completion."})
			return report
		}
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		output, err := probe(bounded, tool.path, append([]string{}, tool.args...))
		cancel()
		if len(output) > 4096 {
			err = errOutputLimit
		}
		status := "warning"
		if tool.required {
			status = "error"
		}
		check := Check{ID: "utility:" + tool.name, Status: status}
		if err != nil {
			switch {
			case errors.Is(err, os.ErrNotExist):
				check.Message = missingMessage(tool.name)
			case errors.Is(err, context.DeadlineExceeded):
				check.Message = "Utility version check timed out. Review the installed utility locally."
			case errors.Is(err, errOutputLimit):
				check.Message = "Utility version output exceeded the safety limit. Review the installation locally."
			default:
				check.Message = "Utility version could not be safely confirmed. Review its installation and execute permissions locally."
			}
			add(check)
			continue
		}
		found := variant(tool.name, output)
		if found == "" {
			check.Message = wrongVariant(tool.name)
			add(check)
			continue
		}
		check.Status = "ok"
		check.Version = found
		check.Message = "Supported utility variant identified. Record and qualify its behavior before physical use."
		add(check)
	}
	roots := []root{{"state", c.StateDir, true, true}, {"spool", filepath.Join(c.StateDir, "spool"), true, true}}
	for _, group := range []struct {
		prefix string
		paths  map[string]string
		write  bool
	}{{"source", c.Sources, false}, {"restore", c.RestoreRoots, true}, {"legacy", c.LegacyRoots, false}} {
		ids := make([]string, 0, len(group.paths))
		for id := range group.paths {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			roots = append(roots, root{group.prefix + ":" + id, group.paths[id], group.write, false})
		}
	}
	canonical := map[string]string{}
	identities := map[string]string{}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			add(Check{ID: "inspection", Status: "error", Message: "Inspection was cancelled before completion."})
			return report
		}
		check, resolved := inspectRoot(root, c.SpoolReserveBytes)
		add(check)
		if resolved != "" {
			canonical[root.id] = resolved
			identities[root.id] = check.Identity
		}
	}
	ids := make([]string, 0, len(canonical))
	for id := range canonical {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	overlap := false
	for n, id := range ids {
		for _, other := range ids[n+1:] {
			if id == "spool" && other == "state" || id == "state" && other == "spool" {
				continue
			}
			a, b := canonical[id], canonical[other]
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") || identities[id] != "" && identities[id] == identities[other] {
				overlap = true
			}
		}
	}
	if overlap {
		add(Check{ID: "roots:overlap", Status: "error", Message: "Configured source, restore, legacy and state roots overlap. Configure separate roots."})
	} else {
		add(Check{ID: "roots:overlap", Status: "ok", Message: "Configured operational roots are separate. The private spool is within state storage."})
	}
	if c.PhysicalRead != nil {
		add(Check{ID: "physical:qualification", Status: "warning", Message: "Physical read settings are configured. Run separately authorized profile and device checks before use."})
	} else {
		add(Check{ID: "physical:qualification", Status: "warning", Message: "Physical hardware is unqualified. Simulator mode is active."})
	}
	return report
}
func variant(name, output string) string {
	if len(output) > 4096 {
		return ""
	}
	lower := strings.ToLower(output)
	accepted := false
	switch name {
	case "tar":
		accepted = strings.Contains(lower, "gnu tar")
	case "mbuffer":
		accepted = strings.Contains(lower, "mbuffer")
	case "mt-st":
		accepted = strings.Contains(lower, "mt-st") && !strings.Contains(lower, "gnu cpio") && !strings.Contains(lower, "gnu mt")
	case "lsscsi":
		accepted = strings.Contains(lower, "lsscsi")
	}
	if !accepted {
		return ""
	}
	return versionNumber.FindString(output)
}
func missingMessage(name string) string {
	switch name {
	case "tar":
		return "GNU tar is missing. Install the documented GNU tar package before backup or restore."
	case "mbuffer":
		return "mbuffer is unavailable. The bounded Go record buffer will be used for simulator streaming."
	case "mt-st":
		return "mt-st is unavailable. Physical operation remains gated."
	default:
		return "Optional lsscsi diagnostics are unavailable."
	}
}
func wrongVariant(name string) string {
	if name == "mt-st" {
		return "The mt command is not an identified mt-st build. GNU mt and unknown variants cannot qualify physical tape operation."
	}
	return "The required utility variant or version could not be identified. Review the installed package locally."
}

type root struct {
	id, path       string
	write, private bool
}

func inspectRoot(item root, reserve int64) (Check, string) {
	check := Check{ID: "root:" + item.id, Status: "error"}
	if !filepath.IsAbs(item.path) || filepath.Clean(item.path) != item.path || item.path == "/" || reserve < 0 || protectedPath(item.path) {
		check.Message = "The configured root path is unsafe. Review host configuration."
		return check, ""
	}
	// Inspect each configured path component without following aliases. This
	// prevents a misconfigured symlink from redirecting even metadata checks into
	// a device, procfs or sysfs target.
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(item.path, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			check.Message = "The configured root is missing or inaccessible. Create it locally with the documented ownership and permissions."
			return check, ""
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			check.Message = "The configured root contains an alias or non-directory component. Configure its canonical directory."
			return check, ""
		}
	}
	resolved := item.path
	file, err := os.OpenFile(item.path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		check.Message = "The configured root cannot be opened as a directory. Check its ownership and permissions."
		return check, resolved
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		check.Message = "The configured root is not a readable directory."
		return check, resolved
	}
	if item.private && info.Mode().Perm()&0077 != 0 {
		check.Message = "State and spool directories must have private owner-only permissions."
		return check, resolved
	}
	access := uint32(unix.R_OK | unix.X_OK)
	if item.write {
		access |= unix.W_OK
	}
	if err = unix.Faccessat(int(file.Fd()), ".", access, unix.AT_EACCESS); err != nil {
		check.Message = "The service lacks required root access. Grant read/search access to sources and write access to state, spool and restore roots."
		return check, resolved
	}
	var filesystem unix.Statfs_t
	if err = unix.Fstatfs(int(file.Fd()), &filesystem); err != nil {
		check.Message = "Filesystem properties could not be confirmed."
		return check, resolved
	}
	if item.private && (filesystem.Type == unix.NFS_SUPER_MAGIC || filesystem.Type == unix.CIFS_SUPER_MAGIC || filesystem.Type == unix.SMB2_SUPER_MAGIC) {
		check.Message = "SQLite state and private spool require local storage. Configure a supported local filesystem."
		return check, resolved
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		check.Message = "Filesystem root identity could not be confirmed."
		return check, resolved
	}
	check.Identity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	if item.id == "spool" || strings.HasPrefix(item.id, "restore:") {
		var space unix.Statfs_t
		if err = unix.Fstatfs(int(file.Fd()), &space); err != nil || space.Bsize <= 0 {
			check.Message = "Free space could not be read for this filesystem."
			return check, resolved
		}
		available := int64(math.MaxInt64)
		if space.Bavail <= uint64(math.MaxInt64)/uint64(space.Bsize) {
			available = int64(space.Bavail) * space.Bsize
		}
		required := reserve
		check.AvailableBytes = &available
		check.RequiredBytes = &required
		if available < reserve {
			check.Message = "Free space is below the configured reserve. Free space before starting restore work."
			return check, resolved
		}
	}
	check.Status = "ok"
	check.Message = "Configured directory identity and service access confirmed."
	return check, resolved
}
func protectedPath(name string) bool {
	for _, prefix := range []string{"/dev", "/sys", "/proc"} {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	return false
}
