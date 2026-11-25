// Package restore validates archive graphs and publishes fresh restored trees.
package restore

import (
	"archive/tar"
	"context"
	"errors"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape/sim"
)

var (
	ErrUnsafe      = domain.ErrUnsafeArchive
	ErrLimit       = errors.New("RESTORE_LIMIT")
	ErrDestination = domain.ErrRestoreDestinationUnavailable
	ErrSpace       = domain.ErrInsufficientSpace
	ErrRoot        = errors.New("RESTORE_REQUIRES_NON_ROOT")
	slugPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
)

const maxMembers = 1000000
const maxMetadataBytes = 64 * 1024 * 1024

type Options struct {
	ArchiveID          string  `json:"archive_id"`
	MemberIDs          []int64 `json:"member_ids"`
	All                bool    `json:"all"`
	RestoreRootID      string  `json:"restore_root_id"`
	DestinationSlug    string  `json:"destination_slug"`
	LegacySourceID     string  `json:"legacy_source_id"`
	LegacyProfile      string  `json:"legacy_profile"`
	LegacyConfirmation bool    `json:"legacy_confirmation"`
}
type Restore struct {
	Store        *store.Store
	Drive        *sim.Drive
	Roots        map[string]string
	LegacyRoots  []legacy.Root
	Spool        string
	ReserveBytes int64
	testHook     func(string) error
}
type PreviewMember struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}
type Plan struct {
	IntegrityBasis   string          `json:"integrity_basis"`
	MetadataPolicy   string          `json:"metadata_policy"`
	ArchiveID        string          `json:"archive_id"`
	VolumeID         string          `json:"volume_id"`
	RestoreRootID    string          `json:"restore_root_id"`
	DestinationSlug  string          `json:"destination_slug"`
	RequestedMembers int             `json:"requested_members"`
	SelectedMembers  int             `json:"selected_members"`
	ExpandedMembers  int             `json:"expanded_members"`
	OutputBytes      int64           `json:"output_bytes"`
	SpoolBytes       int64           `json:"spool_bytes"`
	Members          []PreviewMember `json:"members"`
	Truncated        bool            `json:"truncated"`
	archive          store.Archive
	volume           store.Volume
	all              map[string]store.Member
	selected         map[string]store.Member
	order            []string
	root             string
}

func canonical(raw string) (string, error) {
	if raw == "." || raw == "./" {
		return ".", nil
	}
	if strings.HasPrefix(raw, "./") {
		raw = strings.TrimPrefix(raw, "./")
	}
	raw = strings.TrimSuffix(raw, "/")
	if raw == "" || raw == "." || !utf8.ValidString(raw) || len(raw) > 4096 || strings.ContainsRune(raw, 0) || strings.HasPrefix(raw, "/") || path.Clean(raw) != raw {
		return "", ErrUnsafe
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." || part == "." || part == "" {
			return "", ErrUnsafe
		}
	}
	return raw, nil
}
func graph(members []store.Member) (map[string]store.Member, []string, error) {
	return graphContext(context.Background(), members)
}
func graphContext(ctx context.Context, members []store.Member) (map[string]store.Member, []string, error) {
	nodes := make(map[string]store.Member, len(members))
	order := make([]string, 0, len(members))
	metadataBytes := 0
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		name, err := canonical(m.RawPath)
		if err != nil {
			return nil, nil, err
		}
		if len(nodes) >= maxMembers || metadataBytes > maxMetadataBytes-len(m.RawPath)-len(m.LinkTarget) {
			return nil, nil, ErrLimit
		}
		metadataBytes += len(m.RawPath) + len(m.LinkTarget)
		if _, exists := nodes[name]; exists || m.Size < 0 || strings.Count(name, "/") >= 128 || (name == "." && m.Type != string(tar.TypeDir)) {
			return nil, nil, ErrUnsafe
		}
		switch m.Type {
		case string(tar.TypeReg), string(tar.TypeDir), string(tar.TypeSymlink), string(tar.TypeLink):
		default:
			return nil, nil, ErrUnsafe
		}
		if m.Type != string(tar.TypeReg) && m.Size != 0 {
			return nil, nil, ErrUnsafe
		}
		if (m.Type == string(tar.TypeSymlink) || m.Type == string(tar.TypeLink)) && (!utf8.ValidString(m.LinkTarget) || m.LinkTarget == "" || strings.ContainsRune(m.LinkTarget, 0) || len(m.LinkTarget) > 4096) {
			return nil, nil, ErrUnsafe
		}
		m.Path = name
		nodes[name] = m
		order = append(order, name)
	}
	for name, m := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if n, exists := nodes[parent]; exists && n.Type != string(tar.TypeDir) {
				return nil, nil, ErrUnsafe
			}
		}
		if m.Type == string(tar.TypeSymlink) {
			if err := resolveSymlink(nodes, name); err != nil {
				return nil, nil, err
			}
		}

		if m.Type == string(tar.TypeLink) {
			if _, err := linkTarget(nodes, name); err != nil {
				return nil, nil, err
			}
		}
	}
	return nodes, order, nil
}
func resolveSymlink(nodes map[string]store.Member, name string) error {
	target := nodes[name].LinkTarget
	if strings.HasPrefix(target, "/") {
		return ErrUnsafe
	}
	pending := strings.Split(path.Join(path.Dir(name), "placeholder"), "/")
	pending = pending[:len(pending)-1]
	pending = append(pending, strings.Split(target, "/")...)
	current := []string{}
	links := 0
	for len(pending) > 0 {
		segment := pending[0]
		pending = pending[1:]
		switch segment {
		case "", ".":
			continue
		case "..":
			if len(current) == 0 {
				return ErrUnsafe
			}
			current = current[:len(current)-1]
			continue
		}
		candidate := strings.Join(append(append([]string{}, current...), segment), "/")
		if node, exists := nodes[candidate]; exists && node.Type == string(tar.TypeSymlink) {
			if links >= 40 || strings.HasPrefix(node.LinkTarget, "/") {
				return ErrUnsafe
			}
			links++
			pending = append(strings.Split(node.LinkTarget, "/"), pending...)
			continue
		}
		current = append(current, segment)
	}
	return nil
}

func linkTarget(nodes map[string]store.Member, name string) (string, error) {
	seen := map[string]bool{}
	for {
		if len(seen) >= 128 {
			return "", ErrLimit
		}
		if seen[name] {
			return "", ErrUnsafe
		}
		seen[name] = true
		m, exists := nodes[name]
		if !exists {
			return "", ErrUnsafe
		}
		if m.Type == string(tar.TypeReg) {
			return name, nil
		}
		if m.Type != string(tar.TypeLink) {
			return "", ErrUnsafe
		}
		target, err := canonical(m.LinkTarget)
		if err != nil {
			return "", err
		}
		name = target
	}
}
func (r *Restore) Preview(ctx context.Context, options Options) (Plan, error) {
	var p Plan
	if r.Store == nil || r.ReserveBytes < 0 || !slugPattern.MatchString(options.DestinationSlug) || len(options.MemberIDs) > 10000 || options.All == (len(options.MemberIDs) > 0) {
		return p, ErrUnsafe
	}
	root, exists := r.Roots[options.RestoreRootID]
	if !exists || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return p, ErrDestination
	}
	validatedRoot, err := trustedRoot(root)
	if err != nil {
		return p, err
	}
	_, targetErr := validatedRoot.Lstat(options.DestinationSlug)
	closeErr := validatedRoot.Close()
	if closeErr != nil {
		return p, closeErr
	}
	if !errors.Is(targetErr, os.ErrNotExist) {
		return p, ErrDestination
	}
	archive, err := r.Store.GetArchive(ctx, options.ArchiveID)
	if err != nil {
		return p, err
	}
	if archive.Integrity != domain.Verified && archive.Integrity != domain.Observed || archive.StreamBytes <= 0 {
		return p, ErrUnsafe
	}
	v, err := r.Store.GetVolume(ctx, archive.VolumeID)
	if err != nil {
		return p, err
	}
	if v.Kind != "MANAGED" && v.Kind != "LEGACY" {
		return p, ErrUnsafe
	}
	if v.Kind == "MANAGED" && (archive.Integrity != domain.Verified || archive.CommitIndex == nil || archive.CommitSHA256 == "") {
		return p, ErrUnsafe
	}
	members := make([]store.Member, 0)
	var after int64
	var metadataBytes int
	for {
		if err = ctx.Err(); err != nil {
			return p, err
		}
		batch, err := r.Store.ListMembers(ctx, archive.ID, after, 500)
		if err != nil {
			return p, err
		}
		if len(batch) == 0 {
			break
		}
		if len(members) > maxMembers-len(batch) {
			return p, ErrLimit
		}
		for _, member := range batch {
			n := len(member.RawPath) + len(member.Path) + len(member.ParentPath) + len(member.LinkTarget)
			if n > maxMetadataBytes-metadataBytes {
				return p, ErrLimit
			}
			metadataBytes += n
		}
		members = append(members, batch...)
		after = batch[len(batch)-1].ID
	}
	nodes, order, err := graphContext(ctx, members)
	if err != nil {
		return p, err
	}
	if len(nodes) == 0 {
		return p, ErrUnsafe
	}
	selected := map[string]store.Member{}
	if options.All {
		for name, m := range nodes {
			if name != "." {
				selected[name] = m
			}
		}
	} else {
		wanted := map[int64]bool{}
		for _, id := range options.MemberIDs {
			if id <= 0 || wanted[id] {
				return p, ErrUnsafe
			}
			wanted[id] = true
		}
		prefixes := []string{}
		for name, m := range nodes {
			if !wanted[m.ID] {
				continue
			}
			delete(wanted, m.ID)
			if name != "." {
				selected[name] = m
			}
			if m.Type == string(tar.TypeDir) {
				if name == "." {
					prefixes = append(prefixes, "")
					continue
				}
				prefixes = append(prefixes, name+"/")
			}
		}
		if len(wanted) != 0 {
			return p, ErrUnsafe
		}
		sort.Strings(prefixes)
		compact := prefixes[:0]
		for _, prefix := range prefixes {
			if len(compact) == 0 || !strings.HasPrefix(prefix, compact[len(compact)-1]) {
				compact = append(compact, prefix)
			}
		}
		for name, m := range nodes {
			if name == "." {
				continue
			}
			index := sort.Search(len(compact), func(i int) bool { return compact[i] > name }) - 1
			if index >= 0 && strings.HasPrefix(name, compact[index]) {
				selected[name] = m
			}
		}
	}
	if len(selected) == 0 {
		return p, ErrUnsafe
	}
	requested := len(options.MemberIDs)
	if options.All {
		requested = len(selected)
	}
	// Expand hardlink dependencies and recorded parent directories. Implicit
	// parent directories are separately allowed by the post-extraction checker.
	queue := make([]string, 0, len(selected))
	for name := range selected {
		queue = append(queue, name)
	}
	for index := 0; index < len(queue); index++ {
		if err = ctx.Err(); err != nil {
			return p, err
		}
		name := queue[index]
		m := nodes[name]
		for m.Type == string(tar.TypeLink) {
			target, err := canonical(m.LinkTarget)
			if err != nil {
				return p, err
			}
			m = nodes[target]
			if _, exists := selected[target]; !exists {
				selected[target] = m
				queue = append(queue, target)
			}
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if m, exists := nodes[parent]; exists {
				selected[parent] = m
			}
		}
	}

	p = Plan{IntegrityBasis: archive.IntegrityBasis, MetadataPolicy: "Ordinary permissions only. Ownership, ACLs, extended attributes and privileged bits are omitted.", ArchiveID: archive.ID, VolumeID: v.ID, RestoreRootID: options.RestoreRootID, DestinationSlug: options.DestinationSlug, RequestedMembers: requested, SelectedMembers: len(selected), ExpandedMembers: len(selected) - requested, SpoolBytes: archive.StreamBytes, Members: []PreviewMember{}, archive: archive, volume: v, all: nodes, selected: selected, order: order, root: root}
	for _, name := range order {
		m, exists := selected[name]
		if !exists {
			continue
		}
		if m.Type == string(tar.TypeReg) {
			if p.OutputBytes > math.MaxInt64-m.Size {
				return Plan{}, ErrLimit
			}
			p.OutputBytes += m.Size
		}
		if len(p.Members) < 200 {
			p.Members = append(p.Members, PreviewMember{m.ID, m.Path, m.Type, m.Size})
		} else {
			p.Truncated = true
		}
	}
	return p, nil
}
