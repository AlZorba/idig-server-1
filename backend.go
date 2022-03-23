package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type Backend struct {
	User   string
	Trench string
	r      *git.Repository
}

func NewBackend(root, user, trench string) (*Backend, error) {
	gitDir := filepath.Join(root, trench)
	r, err := git.PlainOpen(gitDir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		r, err = git.PlainInit(gitDir, true)
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to open repository for '%s': %w", trench, err)
	}
	b := &Backend{
		User:   user,
		Trench: trench,
		r:      r,
	}
	return b, nil
}

func (b *Backend) ExistsAttachment(name, checksum string) bool {
	refName := b.attachmentReference(name, checksum)
	_, err := b.r.Reference(plumbing.ReferenceName(refName), false)
	return err == nil
}

type TrenchVersion struct {
	Version string    `json:"version"`
	Date    time.Time `json:"date"`
}

func (b *Backend) ListVersions() ([]TrenchVersion, error) {
	var versions []TrenchVersion
	it, err := b.r.Log(&git.LogOptions{})
	if err != nil {
		return nil, fmt.Errorf("Error getting version list: %w", err)
	}
	err = it.ForEach(func(c *object.Commit) error {
		version := TrenchVersion{
			Version: c.Hash.String(),
			Date:    c.Author.When,
		}
		versions = append(versions, version)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("Error getting version list: %w", err)
	}
	return versions, nil
}

func (b *Backend) ReadAttachment(name, checksum string) ([]byte, error) {
	refName := b.attachmentReference(name, checksum)
	ref, err := b.r.Reference(plumbing.ReferenceName(refName), false)
	if err != nil {
		return nil, fmt.Errorf("Attachment '%s' not found: %w", name, err)
	}
	return b.readBlob(ref.Hash())
}

func (b *Backend) ReadPreferences() ([]byte, error) {
	head := b.Head()
	if head == "" {
		return nil, nil
	} else {
		return b.ReadPreferencesAtVersion(head)
	}
}

func (b *Backend) ReadPreferencesAtVersion(version string) ([]byte, error) {
	h := plumbing.NewHash(version)
	commit, err := b.r.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("Invalid version %s", version)
	}
	rootTree, err := b.r.TreeObject(commit.TreeHash)
	if err != nil {
		return nil, err
	}
	entry, err := rootTree.FindEntry("Preferences.json")
	if err != nil {
		return nil, err
	}
	return b.readBlob(entry.Hash)
}

func (b *Backend) ReadSurveys() ([]Survey, error) {
	head := b.Head()
	if head == "" {
		return nil, nil
	} else {
		return b.ReadSurveysAtVersion(head)
	}
}

func (b *Backend) ReadSurveysAtVersion(version string) ([]Survey, error) {
	h := plumbing.NewHash(version)
	commit, err := b.r.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("Invalid version %s", version)
	}
	rootTree, err := b.r.TreeObject(commit.TreeHash)
	if err != nil {
		return nil, err
	}
	surveysTree, err := rootTree.Tree("surveys")
	if err != nil {
		return nil, err
	}
	var surveys []Survey
	for _, e := range surveysTree.Entries {
		if strings.HasPrefix(e.Name, ".") || !e.Mode.IsFile() {
			log.Printf("Warning: skipping %s", e.Name)
			continue
		}
		data, err := b.readBlob(e.Hash)
		if err != nil {
			return nil, fmt.Errorf("Error reading survey %s: %w", e.Name, err)
		}
		var survey Survey
		err = json.Unmarshal(data, &survey)
		if err != nil {
			return nil, fmt.Errorf("Error reading survey %s: %w", e.Name, err)
		}
		surveys = append(surveys, survey)
	}
	return surveys, nil
}

func (b *Backend) ReadSurveyAtVersion(id, version string) (Survey, error) {
	h := plumbing.NewHash(version)
	commit, err := b.r.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("Invalid version %s", version)
	}
	rootTree, err := b.r.TreeObject(commit.TreeHash)
	if err != nil {
		return nil, err
	}
	surveysTree, err := rootTree.Tree("surveys")
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s.survey", id)
	for _, e := range surveysTree.Entries {
		if e.Name == name {
			data, err := b.readBlob(e.Hash)
			if err != nil {
				return nil, fmt.Errorf("Error reading survey %s: %w", id, err)
			}
			var survey Survey
			err = json.Unmarshal(data, &survey)
			return survey, err
		}
	}
	return nil, fmt.Errorf("Survey %s not found", id)
}

func (b *Backend) ReadAllSurveyVersions(id string) ([]SurveyVersion, error) {
	var versions []SurveyVersion
	filename := fmt.Sprintf("surveys/%s.survey", id)
	lo := &git.LogOptions{
		FileName: &filename,
	}
	it, err := b.r.Log(lo)
	if err != nil {
		return nil, fmt.Errorf("Error getting version list: %w", err)
	}
	err = it.ForEach(func(c *object.Commit) error {
		v := c.Hash.String()
		s, err := b.ReadSurveyAtVersion(id, v)
		if err != nil {
			return err
		}
		version := SurveyVersion{
			Version: v,
			Date:    c.Author.When,
			Survey:  s,
		}
		versions = append(versions, version)
		return nil
	})
	return versions, err
}

func (b *Backend) WriteAttachment(name, checksum string, data []byte) error {
	h, err := b.addBlob(data)
	if err != nil {
		return err
	}

	refName := b.attachmentReference(name, checksum)
	ref := plumbing.NewReferenceFromStrings(refName, h.String())
	err = b.r.Storer.SetReference(ref)
	if err != nil {
		return err
	}

	return nil
}

func (b *Backend) WriteTrench(device, message string, preferences []byte, surveys []Survey) (string, error) {
	var surveyEntries []object.TreeEntry
	var attachmentEntries []object.TreeEntry

	preferencesHash, err := b.addBlob(preferences)
	if err != nil {
		return "", fmt.Errorf("Failed to write preferences: %w", err)
	}

	for _, survey := range surveys {
		id := survey.ID()
		data, err := json.MarshalIndent(survey, "", "  ")
		if err != nil {
			return "", fmt.Errorf("Failed to write survey %s: %w", id, err)
		}
		name := id + ".survey"
		h, err := b.addBlob(data)
		if err != nil {
			return "", fmt.Errorf("Failed to write survey %s data: %w", id, err)
		}
		e := object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h}
		surveyEntries = append(surveyEntries, e)

		for _, a := range survey.Attachments() {
			refName := b.attachmentReference(a.Name, a.Checksum)
			ref, err := b.r.Reference(plumbing.ReferenceName(refName), false)
			if err != nil {
				return "", err
			}
			_ = ref
			e := object.TreeEntry{Name: a.Name, Mode: filemode.Regular, Hash: ref.Hash()}
			attachmentEntries = append(attachmentEntries, e)
		}
	}
	surveysTree, err := b.addTree(surveyEntries)
	if err != nil {
		return "", err
	}
	attachmentsTree, err := b.addTree(attachmentEntries)
	if err != nil {
		return "", err
	}

	rootEntries := []object.TreeEntry{
		{Name: "attachments", Mode: filemode.Dir, Hash: attachmentsTree},
		{Name: "surveys", Mode: filemode.Dir, Hash: surveysTree},
		{Name: "Preferences.json", Mode: filemode.Regular, Hash: preferencesHash},
	}
	rootTree, err := b.addTree(rootEntries)
	if err != nil {
		return "", err
	}

	var parents []plumbing.Hash

	// Check if our root tree is different than head
	head, err := b.r.Head()
	if err == nil {
		c, err := b.r.CommitObject(head.Hash())
		if err != nil {
			return "", err
		}
		parents = append(parents, c.Hash)
		if c.TreeHash == rootTree {
			return c.Hash.String(), nil
		}
	}
	commit, err := b.addCommit(b.User, device, message, rootTree, parents)
	if err != nil {
		return "", err
	}
	err = b.updateHEAD(commit)
	if err != nil {
		return "", err
	}
	return commit.String(), nil
}

func (b *Backend) attachmentReference(name, checksum string) string {
	enc := base64.URLEncoding.WithPadding(base64.NoPadding)
	h := enc.EncodeToString([]byte(fmt.Sprintf("%s/%s", name, checksum)))
	return fmt.Sprintf("refs/attachments/%s", h)
}

func (b *Backend) addBlob(data []byte) (plumbing.Hash, error) {
	obj := b.r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))

	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer w.Close()

	n, err := w.Write(data)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if n != len(data) {
		return plumbing.ZeroHash, fmt.Errorf("Error writing blob data")
	}

	return b.r.Storer.SetEncodedObject(obj)
}

func (b *Backend) addCommit(user, device, message string, tree plumbing.Hash, parents []plumbing.Hash) (plumbing.Hash, error) {
	author := object.Signature{
		Name:  device,
		Email: user,
		When:  time.Now(),
	}
	commit := object.Commit{
		Author:       author,
		Committer:    author,
		Message:      message,
		TreeHash:     tree,
		ParentHashes: parents,
	}
	obj := b.r.Storer.NewEncodedObject()
	err := commit.Encode(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return b.r.Storer.SetEncodedObject(obj)
}

func (b *Backend) addTree(entries []object.TreeEntry) (plumbing.Hash, error) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	tree := object.Tree{Entries: entries}
	obj := b.r.Storer.NewEncodedObject()
	err := tree.Encode(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return b.r.Storer.SetEncodedObject(obj)
}

func (b *Backend) readBlob(h plumbing.Hash) ([]byte, error) {
	blob, err := b.r.BlobObject(h)
	if err != nil {
		return nil, err
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (b *Backend) updateHEAD(commit plumbing.Hash) error {
	name := plumbing.HEAD
	head, err := b.r.Storer.Reference(name)
	if err != nil {
		return err
	}

	if head.Type() != plumbing.HashReference {
		name = head.Target()
	}

	ref := plumbing.NewHashReference(name, commit)
	return b.r.Storer.SetReference(ref)
}

func (b *Backend) Head() string {
	ref, err := b.r.Head()
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

type Survey map[string]string

type Attachment struct {
	Name     string
	Checksum string
}

type SurveyMap map[string]Survey

type SurveyVersion struct {
	Version string    `json:"version"`
	Date    time.Time `json:"date"`
	Survey  Survey    `json:"survey"`
}

func (s Survey) ID() string {
	id := s["IdentifierUUID"]
	if id == "" {
		log.Printf("Warning: Missing survey id: %+v", s)
	}
	return id
}

func (s Survey) Attachments() []Attachment {
	var attachments []Attachment
	for _, a := range strings.Split(s["RelationAttachments"], "\n\n") {
		var name, ts string
		for _, s := range strings.Split(a, "\n") {
			key, val := Cut(s, "=")
			if key == "n" {
				name = val
			} else if key == "d" {
				ts = val
			}
		}
		if name != "" && ts != "" {
			attachment := Attachment{Name: name, Checksum: ts}
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func (s Survey) IsEqual(t Survey) bool {
	for key := range s.Keys().Union(t.Keys()) {
		if s[key] != t[key] {
			return false
		}
	}
	return true
}

func (s Survey) Keys() Set {
	keys := make(Set, len(s))
	for key := range s {
		keys[key] = struct{}{}
	}
	return keys
}

func NewSurveyMap(surveys []Survey) SurveyMap {
	m := make(map[string]Survey)
	for _, s := range surveys {
		id := s.ID()
		m[id] = s
	}
	return m
}

func (m SurveyMap) IDs() Set {
	ids := make(Set, len(m))
	for id := range m {
		ids[id] = struct{}{}
	}
	return ids
}
