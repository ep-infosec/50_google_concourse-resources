// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Logic to interact with a Gerrit server. Gerrit has an entire Git-based
// protocol for fetching metadata about CL's, reviewers, patch comments, which
// is used here - we don't use the x/build/gerrit client, which hits the API.
// TODO: write about Gerrit's Git API.

package maintner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/build/maintner/maintpb"
)

// Gerrit holds information about a number of Gerrit projects.
type Gerrit struct {
	c        *Corpus
	projects map[string]*GerritProject // keyed by "go.googlesource.com/build"

	clsReferencingGithubIssue map[GitHubIssueRef][]*GerritCL
}

func normalizeGerritServer(server string) string {
	u, err := url.Parse(server)
	if err == nil && u.Host != "" {
		server = u.Host
	}
	if strings.HasSuffix(server, "-review.googlesource.com") {
		// special case: the review site is hosted at a different URL than the
		// Git checkout URL.
		return strings.Replace(server, "-review.googlesource.com", ".googlesource.com", 1)
	}
	return server
}

// Project returns the specified Gerrit project if it's known, otherwise
// it returns nil. Server is the Gerrit server's hostname, such as
// "go.googlesource.com".
func (g *Gerrit) Project(server, project string) *GerritProject {
	server = normalizeGerritServer(server)
	return g.projects[server+"/"+project]
}

// c.mu must be held
func (g *Gerrit) getOrCreateProject(gerritProj string) *GerritProject {
	proj, ok := g.projects[gerritProj]
	if ok {
		return proj
	}
	proj = &GerritProject{
		gerrit: g,
		proj:   gerritProj,
		cls:    map[int32]*GerritCL{},
		remote: map[gerritCLVersion]GitHash{},
		ref:    map[string]GitHash{},
		commit: map[GitHash]*GitCommit{},
		need:   map[GitHash]bool{},
	}
	g.projects[gerritProj] = proj
	return proj
}

// ForeachProjectUnsorted calls fn for each known Gerrit project.
// Iteration ends if fn returns a non-nil value.
func (g *Gerrit) ForeachProjectUnsorted(fn func(*GerritProject) error) error {
	for _, p := range g.projects {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

// GerritProject represents a single Gerrit project.
type GerritProject struct {
	gerrit          *Gerrit
	proj            string // "go.googlesource.com/net"
	cls             map[int32]*GerritCL
	remote          map[gerritCLVersion]GitHash
	need            map[GitHash]bool
	commit          map[GitHash]*GitCommit
	numLabelChanges int // meta commits with "Label:" updates

	// ref are the non-change refs with keys like "HEAD",
	// "refs/heads/master", "refs/tags/v0.8.0", etc.
	//
	// Notably, this excludes the "refs/changes/*" refs matched by
	// rxChangeRef. Those are in the remote map.
	ref map[string]GitHash
}

// Ref returns a non-change ref, such as "HEAD", "refs/heads/master",
// or "refs/tags/v0.8.0",
// Change refs of the form "refs/changes/*" are not supported.
// The returned hash is the zero value (an empty string) if the ref
// does not exist.
func (gp *GerritProject) Ref(ref string) GitHash {
	return gp.ref[ref]
}

func (gp *GerritProject) gitDir() string {
	return filepath.Join(gp.gerrit.c.getDataDir(), url.PathEscape(gp.proj))
}

// NumLabelChanges returns the number of times vote labels have
// changed in this project. This number is monotonically increasing.
// This is not guaranteed to be accurate; it might overcount.
// It will not undercount.
func (gp *GerritProject) NumLabelChanges() int {
	return gp.numLabelChanges
}

// ServerSlashProject returns the server and project together, such as
// "go.googlesource.com/build".
func (gp *GerritProject) ServerSlashProject() string { return gp.proj }

// Server returns the Gerrit server, such as "go.googlesource.com".
func (gp *GerritProject) Server() string {
	if i := strings.IndexByte(gp.proj, '/'); i != -1 {
		return gp.proj[:i]
	}
	return ""
}

// Project returns the Gerrit project on the server, such as "go" or "crypto".
func (gp *GerritProject) Project() string {
	if i := strings.IndexByte(gp.proj, '/'); i != -1 {
		return gp.proj[i+1:]
	}
	return ""
}

// ForeachNonChangeRef calls fn for each git ref on the server that is
// not a change (code review) ref. In general, these correspond to
// submitted changes.
// fn is called serially with sorted ref names.
// Iteration stops with the first non-nil error returned by fn.
func (gp *GerritProject) ForeachNonChangeRef(fn func(ref string, hash GitHash) error) error {
	refs := make([]string, 0, len(gp.ref))
	for ref := range gp.ref {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if err := fn(ref, gp.ref[ref]); err != nil {
			return err
		}
	}
	return nil
}

// ForeachOpenCL calls fn for each open CL in the repo.
//
// If fn returns an error, iteration ends and ForeachOpenCL returns
// with that error.
//
// The fn function is called serially, with increasingly numbered
// CLs.
func (gp *GerritProject) ForeachOpenCL(fn func(*GerritCL) error) error {
	var s []*GerritCL
	for _, cl := range gp.cls {
		if cl.Status != "new" {
			continue
		}
		s = append(s, cl)
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Number < s[j].Number })
	for _, cl := range s {
		if err := fn(cl); err != nil {
			return err
		}
	}
	return nil
}

// ForeachCLUnsorted calls fn for each CL in the repo, in any order
//
// If fn returns an error, iteration ends and ForeachCLUnsorted returns with
// that error.
func (gp *GerritProject) ForeachCLUnsorted(fn func(*GerritCL) error) error {
	for _, cl := range gp.cls {
		if err := fn(cl); err != nil {
			return err
		}
	}
	return nil
}

// CL returns the GerritCL with the given number, or nil if it is not present.
//
// CL numbers are shared across all projects on a Gerrit server, so you can get
// nil unless you have the GerritProject containing that CL.
func (gp *GerritProject) CL(number int32) *GerritCL {
	return gp.cls[number]
}

// GitCommit returns the provided git commit, or nil if it's unknown.
func (gp *GerritProject) GitCommit(hash string) *GitCommit {
	if len(hash) != 40 {
		// TODO: support prefix lookups. build a trie. But
		// for now just avoid panicking in gitHashFromHexStr.
		return nil
	}
	var buf [20]byte
	_, err := decodeHexStr(buf[:], hash)
	if err != nil {
		return nil
	}
	return gp.commit[GitHash(buf[:])]
}

func (gp *GerritProject) logf(format string, args ...interface{}) {
	log.Printf("gerrit "+gp.proj+": "+format, args...)
}

// gerritCLVersion is a value type used as a map key to store a CL
// number and a patchset version. Its Version field is overloaded
// to reference the "meta" metadata commit if the Version is 0.
type gerritCLVersion struct {
	CLNumber int32
	Version  int32 // version 0 is used for the "meta" ref.
}

// A GerritCL represents a single change in Gerrit.
type GerritCL struct {
	// Project is the project this CL is part of.
	Project *GerritProject

	// Number is the CL number on the Gerrit server (e.g. 1, 2, 3). Gerrit CL
	// numbers are sparse (CL N does not guarantee that CL N-1 exists) and
	// Gerrit issues CL's out of order - it may issue CL N, then CL (N - 18),
	// then CL (N - 40).
	Number int32

	Created time.Time

	// Version is the number of versions of the patchset for this
	// CL seen so far. It starts at 1.
	Version int32

	// Commit is the git commit of the latest version of this CL.
	// Previous versions are available via GerritProject.remote.
	Commit *GitCommit

	// Meta is the head of the most recent Gerrit "meta" commit
	// for this CL. This is guaranteed to be a linear history
	// back to a CL-specific root commit for this meta branch.
	Meta        *GitCommit
	MetaCommits []*GitCommit // in order, from root to Meta

	// Status will be "merged", "abandoned", "new", or "draft".
	Status string

	// GitHubIssueRefs are parsed references to GitHub issues.
	GitHubIssueRefs []GitHubIssueRef

	// Messages contains all of the messages for this CL, in sorted order.
	Messages []*GerritMessage
}

// GerritMetaCommit represents a GitCommit in the Gerrit "NoteDb" format.
type GerritMetaCommit struct {
	*GitCommit
}

// GerritMessage is a Gerrit reply that is attached to the CL as a whole, and
// not to a file or line of a patch set.
//
// Maintner does very little parsing or formatting of a Message body. Messages
// are stored the same way they are stored in the API.
type GerritMessage struct {
	// Version is the patch set version this message was sent on.
	Version int32

	// Message is the raw message contents from Gerrit (a subset
	// of the raw git commit message), starting with "Patch Set
	// nnnn".
	Message string

	// Date is when this message was stored (the commit time of
	// the git commit).
	Date time.Time

	// Author returns the author of the commit. This takes the form "Kevin Burke
	// <13437@62eb7196-b449-3ce5-99f1-c037f21e1705>", where the number before
	// the '@' sign is your Gerrit user ID, and the UUID after the '@' sign
	// seems to be the same for all commits for the same Gerrit server, across
	// projects.
	//
	// TODO: Merge the *GitPerson object here and for a person's Git commits
	// (which use their real email) via the user ID, so they point to the same
	// object.
	Author *GitPerson
}

// References reports whether cl includes a commit message reference
// to the provided Github issue ref.
func (cl *GerritCL) References(ref GitHubIssueRef) bool {
	for _, eref := range cl.GitHubIssueRefs {
		if eref == ref {
			return true
		}
	}
	return false
}

// Branch returns the CL's branch, with any "refs/heads/" prefix removed.
func (cl *GerritCL) Branch() string {
	branch, _ := lineValue(cl.firstMetaCommit().Msg, "Branch:")
	return branch
}

// lineValue extracts a value from an RFC 822-style "key: value" series of lines.
// If all is,
//    foo: bar
//    bar: baz
// lineValue(all, "foo:") returns "bar". It trims any whitespace.
// The prefix is case sensitive and must include the colon.
func lineValue(all, prefix string) (value, rest string) {
	orig := all
	consumed := 0
	for {
		i := strings.Index(all, prefix)
		if i == -1 {
			return "", ""
		}
		if i > 0 && all[i-1] != '\n' && all[i-1] != '\r' {
			all = all[i+len(prefix):]
			consumed += i + len(prefix)
			continue
		}
		val := all[i+len(prefix):]
		consumed += i + len(prefix)
		if nl := strings.IndexByte(val, '\n'); nl != -1 {
			consumed += nl + 1
			val = val[:nl+1]
		} else {
			consumed = len(orig)
		}
		return strings.TrimSpace(val), orig[consumed:]
	}
}

// ChangeID returns the Gerrit "Change-Id: Ixxxx" line's Ixxxx
// value from the gc.Msg, if any.
func (gc *GerritCL) ChangeID() string {
	id, _ := lineValue(gc.Commit.Msg, "Change-Id:")
	if strings.HasPrefix(id, "I") && len(id) == 41 {
		return id
	}
	return ""
}

// OwnerID returns the ID of the CL???s owner. It will return -1 on error.
func (cl *GerritCL) OwnerID() int {
	// Meta commits caused by the owner of a change have an email of the form
	// <user id>@<uuid of gerrit server>.
	m := cl.firstMetaCommit()
	if m == nil {
		return -1
	}
	email := m.Author.Email()
	idx := strings.Index(email, "@")
	if idx == -1 {
		return -1
	}
	id, err := strconv.Atoi(email[:idx])
	if err != nil {
		return -1
	}
	return id
}

// Subject returns the first line of the latest commit message.
func (cl *GerritCL) Subject() string {
	if cl.Commit == nil {
		return ""
	}
	if i := strings.Index(cl.Commit.Msg, "\n"); i >= 0 {
		return cl.Commit.Msg[:i]
	}
	return cl.Commit.Msg
}

func (cl *GerritCL) firstMetaCommit() *GitCommit {
	m := cl.Meta
	for {
		if m == nil || len(m.Parents) == 0 {
			return m
		}
		m = m.Parents[0] // Meta commits don???t have more than one parent.
	}
}

func (cl *GerritCL) updateGithubIssueRefs() {
	gp := cl.Project
	gerrit := gp.gerrit
	gc := cl.Commit

	oldRefs := cl.GitHubIssueRefs
	newRefs := gerrit.c.parseGithubRefs(gp.proj, gc.Msg)
	cl.GitHubIssueRefs = newRefs
	for _, ref := range newRefs {
		if !clSliceContains(gerrit.clsReferencingGithubIssue[ref], cl) {
			// TODO: make this as small as
			// possible? Most will have length
			// 1. Care about default capacity of
			// 2?
			gerrit.clsReferencingGithubIssue[ref] = append(gerrit.clsReferencingGithubIssue[ref], cl)
		}
	}
	for _, ref := range oldRefs {
		if !cl.References(ref) {
			// TODO: remove ref from gerrit.clsReferencingGithubIssue
			// It could be a map of maps I suppose, but not as compact.
			// So uses a slice as the second layer, since there will normally
			// be one item.
		}
	}
}

// c.mu must be held
func (c *Corpus) initGerrit() {
	if c.gerrit != nil {
		return
	}
	c.gerrit = &Gerrit{
		c:                         c,
		projects:                  map[string]*GerritProject{},
		clsReferencingGithubIssue: map[GitHubIssueRef][]*GerritCL{},
	}
}

type watchedGerritRepo struct {
	project *GerritProject
}

// TrackGerrit registers the Gerrit project with the given project as a project
// to watch and append to the mutation log. Only valid in leader mode.
// The provided string should be of the form "hostname/project", without a scheme
// or trailing slash.
func (c *Corpus) TrackGerrit(gerritProj string) {
	if c.mutationLogger == nil {
		panic("can't TrackGerrit in non-leader mode")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if strings.Count(gerritProj, "/") != 1 {
		panic(fmt.Sprintf("gerrit project argument %q expected to contain exactly 1 slash", gerritProj))
	}
	c.initGerrit()
	if _, dup := c.gerrit.projects[gerritProj]; dup {
		panic("duplicated watched gerrit project " + gerritProj)
	}
	project := c.gerrit.getOrCreateProject(gerritProj)
	if project == nil {
		panic("gerrit project not created")
	}
	c.watchedGerritRepos = append(c.watchedGerritRepos, watchedGerritRepo{
		project: project,
	})
}

// called with c.mu Locked
func (c *Corpus) processGerritMutation(gm *maintpb.GerritMutation) {
	if c.gerrit == nil {
		// TODO: option to ignore mutation if user isn't interested.
		c.initGerrit()
	}
	gp, ok := c.gerrit.projects[gm.Project]
	if !ok {
		// TODO: option to ignore mutation if user isn't interested.
		// For now, always process the record.
		gp = c.gerrit.getOrCreateProject(gm.Project)
	}
	gp.processMutation(gm)
}

var statusIndicator = "\nStatus: "

// The Go Gerrit site does not really use the "draft" status much, but if
// you need to test it, create a dummy commit and then run
//
//     git push origin HEAD:refs/drafts/master
var statuses = []string{"merged", "abandoned", "draft", "new"}

// getGerritStatus returns a Gerrit status for a commit, or the empty string to
// indicate the commit did not show a status.
//
// getGerritStatus relies on the Gerrit code review convention of amending
// the meta commit to include the current status of the CL. The Gerrit search
// bar allows you to search for changes with the following statuses: "open",
// "reviewed", "closed", "abandoned", "merged", "draft", "pending". The REST API
// returns only "NEW", "DRAFT", "ABANDONED", "MERGED". Gerrit attaches "draft",
// "abandoned", "new", and "merged" statuses to some meta commits; you may have
// to search the current meta commit's parents to find the last good commit.
func getGerritStatus(commit *GitCommit) string {
	idx := strings.Index(commit.Msg, statusIndicator)
	if idx == -1 {
		return ""
	}
	off := idx + len(statusIndicator)
	for _, status := range statuses {
		if strings.HasPrefix(commit.Msg[off:], status) {
			return status
		}
	}
	return ""
}

var errStopIteration = errors.New("stop iteration")
var errTooManyParents = errors.New("maintner: too many commit parents")

// foreachCommitParent walks a commit's parents, calling f for each commit until
// an error is returned from f or a commit has no parent.
//
// foreachCommitParent returns errTooManyParents (and stops processing) if a commit
// has more than one parent.
//
// Corpus.mu must be held.
func (gp *GerritProject) foreachCommitParent(hash GitHash, f func(*GitCommit) error) error {
	c := gp.gerrit.c
	commit := c.gitCommit[hash]
	for {
		if commit == nil {
			return nil
		}
		if err := f(commit); err != nil {
			return err
		}
		if commit.Parents == nil || len(commit.Parents) == 0 {
			return nil
		}
		if len(commit.Parents) > 1 {
			return errTooManyParents
		}
		parentHash := commit.Parents[0].Hash // meta tree has no merge commits
		commit = c.gitCommit[parentHash]
	}
}

// getGerritMessage parses a Gerrit comment from the given commit or returns nil
// if there wasn't one.
//
// Corpus.mu must be held.
func (gp *GerritProject) getGerritMessage(commit *GitCommit) *GerritMessage {
	const existVerPhrase = "\nPatch Set "
	const newVerPhrase = "\nUploaded patch set "

	startExist := strings.Index(commit.Msg, existVerPhrase)
	startNew := strings.Index(commit.Msg, newVerPhrase)
	var start int
	var phrase string
	switch {
	case startExist == -1 && startNew == -1:
		return nil
	case startExist == -1 || (startNew != -1 && startNew < startExist):
		phrase = newVerPhrase
		start = startNew
	case startNew == -1 || (startExist != -1 && startExist < startNew):
		phrase = existVerPhrase
		start = startExist
	}

	numStart := start + len(phrase)
	colon := strings.IndexByte(commit.Msg[numStart:], ':')
	if colon == -1 {
		return nil
	}
	num := commit.Msg[numStart : numStart+colon]
	if strings.Contains(num, "\n") || strings.Contains(num, ".") {
		// Spanned lines. Didn't match expected comment form
		// we care about (comments with vote changes), like:
		//
		//    Uploaded patch set 5: Some-Vote=+2
		//
		// For now, treat such meta updates (new uploads only)
		// as not comments.
		return nil
	}
	version, err := strconv.ParseInt(num, 10, 32)
	if err != nil {
		gp.logf("for phrase %q at %d, unexpected patch set number in %s; err: %v, message: %s", phrase, start, commit.Hash, err, commit.Msg)
		return nil
	}
	start++
	v := commit.Msg[start:]
	l := 0
	for {
		i := strings.IndexByte(v, '\n')
		if i < 0 {
			return nil
		}
		if strings.HasPrefix(v[:i], "Patch-set:") {
			// two newlines before the Patch-set message
			v = commit.Msg[start : start+l-2]
			break
		}
		v = v[i+1:]
		l = l + i + 1
	}
	return &GerritMessage{
		Author:  commit.Author,
		Date:    commit.CommitTime,
		Message: v,
		Version: int32(version),
	}
}

func reverseGerritMessages(ss []*GerritMessage) {
	for i := len(ss)/2 - 1; i >= 0; i-- {
		opp := len(ss) - 1 - i
		ss[i], ss[opp] = ss[opp], ss[i]
	}
}

// called with c.mu Locked
func (gp *GerritProject) processMutation(gm *maintpb.GerritMutation) {
	c := gp.gerrit.c

	for _, refp := range gm.Refs {
		refName := refp.Ref
		hash := c.gitHashFromHexStr(refp.Sha1)
		m := rxChangeRef.FindStringSubmatch(refName)
		if m == nil {
			if strings.HasPrefix(refName, "refs/meta/") {
				// Some of these slipped in to the data
				// before we started ignoring them. So ignore them here.
				continue
			}
			// Misc ref, not a change ref.
			if _, ok := c.gitCommit[hash]; !ok {
				gp.logf("ERROR: non-change ref %v references unknown hash %v; ignoring", refp, hash)
				continue
			}
			gp.ref[refName] = hash
			continue
		}

		clNum64, err := strconv.ParseInt(m[1], 10, 32)
		version, ok := gerritVersionNumber(m[2])
		if !ok || err != nil {
			continue
		}
		gc, ok := c.gitCommit[hash]
		if !ok {
			gp.logf("ERROR: ref %v references unknown hash %v; ignoring", refp, hash)
			continue
		}
		clv := gerritCLVersion{int32(clNum64), version}
		gp.remote[clv] = hash
		cl := gp.getOrCreateCL(clv.CLNumber)
		if clv.Version == 0 {
			oldMeta := cl.Meta
			cl.Meta = gc
			foundStatus := ""
			// Walk from the newest commit backwards, so we store the messages
			// in reverse order and then flip the array before setting on the
			// GerritCL object.
			var backwardMessages []*GerritMessage
			var backwardMeta []*GitCommit
			gp.foreachCommitParent(cl.Meta.Hash, func(gc *GitCommit) error {
				if strings.Contains(gc.Msg, "\nLabel: ") {
					gp.numLabelChanges++
				}
				if oldMeta != nil && gc.Hash == oldMeta.Hash {
					return errStopIteration
				}
				if gc.GerritMeta == nil {
					gc.GerritMeta = &GerritMeta{
						Commit: gc,
						CL:     cl,
					}
				}
				backwardMeta = append(backwardMeta, gc)
				if status := getGerritStatus(gc); status != "" && foundStatus == "" {
					foundStatus = status
				}
				if message := gp.getGerritMessage(gc); message != nil {
					backwardMessages = append(backwardMessages, message)
				}
				return nil
			})
			if foundStatus != "" {
				cl.Status = foundStatus
			} else if cl.Status == "" {
				cl.Status = "new"
			}
			if cl.Created.IsZero() || gc.CommitTime.Before(cl.Created) {
				cl.Created = gc.CommitTime
			}
			reverseGerritMessages(backwardMessages)
			cl.Messages = append(cl.Messages, backwardMessages...)
			for i := len(backwardMeta) - 1; i >= 0; i-- {
				cl.MetaCommits = append(cl.MetaCommits, backwardMeta[i])
			}
		} else {
			cl.Commit = gc
			cl.Version = clv.Version
			cl.updateGithubIssueRefs()
		}
		if c.didInit {
			gp.logf("Ref %+v => %v", clv, hash)
		}
	}

	for _, commitp := range gm.Commits {
		gc, err := c.processGitCommit(commitp)
		if err != nil {
			gp.logf("error processing commit %q: %v", commitp.Sha1, err)
			continue
		}
		gp.commit[gc.Hash] = gc
		delete(gp.need, gc.Hash)

		for _, p := range gc.Parents {
			gp.markNeededCommit(p.Hash)
		}
	}
}

// clSliceContains reports whether cls contains cl.
func clSliceContains(cls []*GerritCL, cl *GerritCL) bool {
	for _, v := range cls {
		if v == cl {
			return true
		}
	}
	return false
}

// c.mu must be held
func (gp *GerritProject) markNeededCommit(hash GitHash) {
	if _, ok := gp.commit[hash]; ok {
		// Already have it.
		return
	}
	gp.need[hash] = true
}

// c.mu must be held
func (gp *GerritProject) getOrCreateCL(num int32) *GerritCL {
	cl, ok := gp.cls[num]
	if ok {
		return cl
	}
	cl = &GerritCL{
		Project: gp,
		Number:  num,
	}
	gp.cls[num] = cl
	return cl
}

func gerritVersionNumber(s string) (version int32, ok bool) {
	if s == "meta" {
		return 0, true
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(v), true
}

// rxRemoteRef matches "git ls-remote" lines.
//
// sample row:
// fd1e71f1594ce64941a85428ddef2fbb0ad1023e	refs/changes/99/30599/3
//
// Capture values:
//   $0: whole match
//   $1: "fd1e71f1594ce64941a85428ddef2fbb0ad1023e"
//   $2: "30599" (CL number)
//   $3: "1", "2" (patchset number) or "meta" (a/ special commit
//       holding the comments for a commit)
//
// The "99" in the middle covers all CL's that end in "99", so
// refs/changes/99/99/1, refs/changes/99/199/meta.
var rxRemoteRef = regexp.MustCompile(`^([0-9a-f]{40,})\s+refs/changes/[0-9a-f]{2}/([0-9]+)/(.+)$`)

// $1: change num
// $2: version or "meta"
var rxChangeRef = regexp.MustCompile(`^refs/changes/[0-9a-f]{2}/([0-9]+)/(meta|(?:\d+))`)

func (gp *GerritProject) sync(ctx context.Context, loop bool) error {
	if err := gp.init(ctx); err != nil {
		gp.logf("init: %v", err)
		return err
	}
	activityCh := gp.gerrit.c.activityChan("gerrit:" + gp.proj)
	for {
		if err := gp.syncOnce(ctx); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				err = fmt.Errorf("%v; stderr=%q", err, ee.Stderr)
			}
			gp.logf("sync: %v", err)
			return err
		}
		if !loop {
			return nil
		}
		timer := time.NewTimer(15 * time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-activityCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// syncMissingCommits is a cleanup step to fix a previous maintner bug where
// refs were updated without all their reachable commits being indexed and
// recorded in the log. This should only ever run once, and only in Go's history.
// If we restarted the log from the beginning this wouldn't be necessary.
func (gp *GerritProject) syncMissingCommits(ctx context.Context) error {
	c := gp.gerrit.c
	var hashes []GitHash
	c.mu.Lock()
	for hash := range gp.need {
		hashes = append(hashes, hash)
	}
	c.mu.Unlock()
	if len(hashes) == 0 {
		return nil
	}

	gp.logf("fixing indexing of %d missing commits", len(hashes))
	if err := gp.fetchHashes(ctx, hashes); err != nil {
		return err
	}

	n, err := gp.syncCommits(ctx)
	if err != nil {
		return err
	}
	gp.logf("%d missing commits indexed", n)
	return nil
}

func (gp *GerritProject) syncOnce(ctx context.Context) error {
	if err := gp.syncMissingCommits(ctx); err != nil {
		return err
	}

	c := gp.gerrit.c
	gitDir := gp.gitDir()

	fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	cmd := exec.CommandContext(fetchCtx, "git", "fetch", "origin")
	cmd.Dir = gitDir
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		return fmt.Errorf("git fetch origin: %v, %s", err, out)
	}

	cmd = exec.CommandContext(ctx, "git", "ls-remote")
	cmd.Dir = gitDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote in %s: %v, %s", gitDir, err, out)
	}

	var changedRefs []*maintpb.GitRef
	var toFetch []GitHash

	bs := bufio.NewScanner(bytes.NewReader(out))

	// Take the lock here to access gp.remote and call c.gitHashFromHex.
	// It's acceptable to take such a coarse-looking lock because
	// it's not actually around I/O: all the input from ls-remote has
	// already been slurped into memory.
	c.mu.Lock()
	for bs.Scan() {
		line := bs.Bytes()
		tab := bytes.IndexByte(line, '\t')
		if tab == -1 {
			if !strings.HasPrefix(bs.Text(), "From ") {
				gp.logf("bogus ls-remote line: %q", line)
			}
			continue
		}
		sha1 := string(line[:tab])
		refName := strings.TrimSpace(string(line[tab+1:]))
		hash := c.gitHashFromHexStr(sha1)

		var needFetch bool

		m := rxRemoteRef.FindSubmatch(line)
		if m != nil {
			clNum, err := strconv.ParseInt(string(m[2]), 10, 32)
			version, ok := gerritVersionNumber(string(m[3]))
			if err != nil || !ok {
				continue
			}
			curHash := gp.remote[gerritCLVersion{int32(clNum), version}]
			needFetch = curHash != hash
		} else if trackGerritRef(refName) && gp.ref[refName] != hash {
			needFetch = true
			gp.logf("gerrit ref %q = %q", refName, sha1)
		}

		if needFetch {
			toFetch = append(toFetch, hash)
			changedRefs = append(changedRefs, &maintpb.GitRef{
				Ref:  refName,
				Sha1: string(sha1),
			})
		}
	}
	c.mu.Unlock()
	if err := bs.Err(); err != nil {
		return err
	}
	if len(changedRefs) == 0 {
		return nil
	}
	gp.logf("%d new refs", len(changedRefs))
	const batchSize = 250
	for len(toFetch) > 0 {
		batch := toFetch
		if len(batch) > batchSize {
			batch = batch[:batchSize]
		}
		if err := gp.fetchHashes(ctx, batch); err != nil {
			return err
		}

		c.mu.Lock()
		for _, hash := range batch {
			gp.markNeededCommit(hash)
		}
		c.mu.Unlock()

		n, err := gp.syncCommits(ctx)
		if err != nil {
			return err
		}
		toFetch = toFetch[len(batch):]
		gp.logf("synced %v commits for %d new hashes, %d hashes remain", n, len(batch), len(toFetch))

		c.addMutation(&maintpb.Mutation{
			Gerrit: &maintpb.GerritMutation{
				Project: gp.proj,
				Refs:    changedRefs[:len(batch)],
			}})
		changedRefs = changedRefs[len(batch):]
	}
	return nil
}

func (gp *GerritProject) syncCommits(ctx context.Context) (n int, err error) {
	c := gp.gerrit.c
	lastLog := time.Now()
	for {
		hash := gp.commitToIndex()
		if hash == "" {
			return n, nil
		}
		now := time.Now()
		if lastLog.Before(now.Add(-1 * time.Second)) {
			lastLog = now
			gp.logf("parsing commits (%v done)", n)
		}
		commit, err := parseCommitFromGit(gp.gitDir(), hash)
		if err != nil {
			return n, err
		}
		c.addMutation(&maintpb.Mutation{
			Gerrit: &maintpb.GerritMutation{
				Project: gp.proj,
				Commits: []*maintpb.GitCommit{commit},
			},
		})
		n++
	}
}

func (gp *GerritProject) commitToIndex() GitHash {
	c := gp.gerrit.c

	c.mu.RLock()
	defer c.mu.RUnlock()
	for hash := range gp.need {
		return hash
	}
	return ""
}

var (
	statusSpace = []byte("Status: ")
)

func (gp *GerritProject) fetchHashes(ctx context.Context, hashes []GitHash) error {
	args := []string{"fetch", "--quiet", "origin"}
	for _, hash := range hashes {
		args = append(args, hash.String())
	}
	gp.logf("fetching %v hashes...", len(hashes))
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = gp.gitDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("error fetching %d hashes from gerrit project %s: %s", len(hashes), gp.proj, out)
		return err
	}
	gp.logf("fetched %v hashes.", len(hashes))
	return nil
}

func formatExecError(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("%v; stderr=%q", err, ee.Stderr)
	}
	return fmt.Sprint(err)
}

func (gp *GerritProject) init(ctx context.Context) error {
	gitDir := gp.gitDir()
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		return err
	}
	// try to short circuit a git init error, since the init error matching is
	// brittle
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("looking for git binary: %v", err)
	}

	if _, err := os.Stat(filepath.Join(gitDir, ".git", "config")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "remote", "-v")
		cmd.Dir = gitDir
		remoteBytes, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("running git remote -v in %v: %v", gitDir, formatExecError(err))
		}
		if !strings.Contains(string(remoteBytes), "origin") && !strings.Contains(string(remoteBytes), "https://"+gp.proj) {
			return fmt.Errorf("didn't find origin & gp.url in remote output %s", string(remoteBytes))
		}
		gp.logf("git directory exists.")
		return nil
	}

	cmd := exec.CommandContext(ctx, "git", "init")
	buf := new(bytes.Buffer)
	cmd.Stdout = buf
	cmd.Stderr = buf
	cmd.Dir = gitDir
	if err := cmd.Run(); err != nil {
		log.Printf(`Error running "git init": %s`, buf.String())
		return err
	}
	buf.Reset()
	cmd = exec.CommandContext(ctx, "git", "remote", "add", "origin", "https://"+gp.proj)
	cmd.Stdout = buf
	cmd.Stderr = buf
	cmd.Dir = gitDir
	if err := cmd.Run(); err != nil {
		log.Printf(`Error running "git remote add origin": %s`, buf.String())
		return err
	}

	return nil
}

// trackGerritRef reports whether we care to record changes about the
// given ref.
func trackGerritRef(ref string) bool {
	if strings.HasPrefix(ref, "refs/users/") {
		return false
	}
	if strings.HasPrefix(ref, "refs/meta/") {
		return false
	}
	if strings.HasPrefix(ref, "refs/cache-automerge/") {
		return false
	}
	return true
}

func (g *Gerrit) check() error {
	for key, gp := range g.projects {
		if err := gp.check(); err != nil {
			return fmt.Errorf("%s: %v", key, err)
		}
	}
	return nil
}

func (gp *GerritProject) check() error {
	if len(gp.need) != 0 {
		return fmt.Errorf("%d missing commits", len(gp.need))
	}
	for hash, gc := range gp.commit {
		if gc.Committer == placeholderCommitter {
			return fmt.Errorf("git commit for key %q was placeholder", hash)
		}
		if gc.Hash != hash {
			return fmt.Errorf("git commit for key %q had GitCommit.Hash %q", hash, gc.Hash)
		}
		for _, pc := range gc.Parents {
			if _, ok := gp.commit[pc.Hash]; !ok {
				return fmt.Errorf("git commit %q exits but its parent %q does not", gc.Hash, pc.Hash)
			}
		}
	}
	return nil
}

// GerritMeta represents a Git commit in the Gerrit NoteDb meta
// format.
type GerritMeta struct {
	// Commit points up to the git commit for this Gerrit NoteDB meta commit.
	Commit *GitCommit

	// CL is the Gerrit CL this metadata is for.
	CL *GerritCL
}

// Footer returns the "key: value" lines at the base of the commit.
func (m *GerritMeta) Footer() string {
	i := strings.LastIndex(m.Commit.Msg, "\n\n")
	if i == -1 {
		return ""
	}
	return m.Commit.Msg[i+2:]
}

// LabelVotes returns a map from label name to voter email to their vote.
//
// This is relatively expensive to call compared to other methods in maintner.
// It is not currently cached.
func (m *GerritMeta) LabelVotes() map[string]map[string]int8 {
	if m == nil {
		panic("nil *GerritMeta")
	}
	if m.CL == nil {
		panic("GerritMeta has nil CL field")
	}
	// To calculate votes as the time of the 'm' meta commit,
	// we need to consider the meta commits before it.
	// Let's see which number in the (linear) meta history
	// we are.
	ourIndex := -1
	for i, mc := range m.CL.MetaCommits {
		if mc.GerritMeta == m {
			ourIndex = i
			break
		}
	}
	if ourIndex == -1 {
		panic("LabelVotes called on GerritMeta not in its m.CL.MetaCommits slice")
	}
	labels := map[string]map[string]int8{}

	history := m.CL.MetaCommits[:ourIndex+1]
	var lastCommit string
	for _, mc := range history {
		log.Printf("For CL %v, mc %v", m.CL.Number, mc.GerritMeta)
		footer := mc.GerritMeta.Footer()
		isNew := strings.Contains(footer, "\nTag: autogenerated:gerrit:newPatchSet\n")
		email := mc.Author.Email()
		if isNew {
			commit, _ := lineValue(footer, "Commit: ")
			if commit != "" {
				// TODO: implement Gerrit's vote copying. For example,
				// label.Label-Name.copyAllScoresIfNoChange defaults to true (as it is with Go's server)
				// https://gerrit-review.googlesource.com/Documentation/config-labels.html#label_copyAllScoresIfNoChange
				// We don't have the information in Maintner to do this, though.
				// One approximation is:
				if lastCommit != "" {
					oldCommit := m.CL.Project.GitCommit(lastCommit)
					newCommit := m.CL.Project.GitCommit(commit)
					if !oldCommit.SameDiffStat(newCommit) {
						// TODO: this should really use
						// the Gerrit server's project
						// config, including the
						// All-Projects config, but
						// that's not in Maintner
						// either.
						delete(labels, "Run-TryBot")
						delete(labels, "TryBot-Result")
					}
				}
				lastCommit = commit
			}
		}

		remain := footer
		for len(remain) > 0 {
			var labelEqVal string
			labelEqVal, remain = lineValue(remain, "Label: ")
			if labelEqVal != "" {
				label, value, whose := parseGerritLabelValue(labelEqVal)
				if label != "" {
					if whose == "" {
						whose = email
					}
					if label[0] == '-' {
						label = label[1:]
						if m := labels[label]; m != nil {
							delete(m, whose)
						}
					} else {
						m := labels[label]
						if m == nil {
							m = make(map[string]int8)
							labels[label] = m
						}
						m[whose] = value

					}
				}
			}
		}
	}

	return labels
}

// parseGerritLabelValue parses a Gerrit NoteDb "Label: ..." value.
// It can take forms and return values such as:
//
//     "Run-TryBot=+1" => ("Run-TryBot", 1, "")
//     "-Run-TryBot" => ("-Run-TryBot", 0, "")
//     "-Run-TryBot " => ("-Run-TryBot", 0, "")
//     "Run-TryBot=+1 Brad Fitzpatrick <5065@62eb7196-b449-3ce5-99f1-c037f21e1705>" =>
//           ("Run-TryBot", 1, "5065@62eb7196-b449-3ce5-99f1-c037f21e1705")
//     "-TryBot-Result Gobot Gobot <5976@62eb7196-b449-3ce5-99f1-c037f21e1705>" =>
//           ("-TryBot-Result", 0, "5976@62eb7196-b449-3ce5-99f1-c037f21e1705")
func parseGerritLabelValue(v string) (label string, value int8, whose string) {
	space := strings.IndexByte(v, ' ')
	if space != -1 {
		v, whose = v[:space], v[space+1:]
		if i := strings.IndexByte(whose, '<'); i == -1 {
			whose = ""
		} else {
			whose = whose[i+1:]
			if i := strings.IndexByte(whose, '>'); i == -1 {
				whose = ""
			} else {
				whose = whose[:i]
			}
		}
	}
	v = strings.TrimSpace(v)
	if eq := strings.IndexByte(v, '='); eq == -1 {
		label = v
	} else {
		label = v[:eq]
		if n, err := strconv.ParseInt(v[eq+1:], 10, 8); err == nil {
			value = int8(n)
		}
	}
	return
}
