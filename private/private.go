package private

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	golog "github.com/ipfs/go-log"
	multihash "github.com/multiformats/go-multihash"
	base "github.com/qri-io/wnfs-go/base"
	public "github.com/qri-io/wnfs-go/public"
	ratchet "github.com/qri-io/wnfs-go/ratchet"
)

var (
	log      = golog.Logger("wnfs")
	EmptyKey = Key([32]byte{})
)

type Key [32]byte

func NewKey() Key {
	return ratchet.NewSpiral().Key()
}

func (k Key) Encode() string { return base64.URLEncoding.EncodeToString(k[:]) }

func (k *Key) Decode(s string) error {
	data, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	for i, d := range data {
		k[i] = d
	}
	return nil
}

func (k Key) IsEmpty() bool { return k == EmptyKey }

func (k Key) MarshalJSON() ([]byte, error) {
	return []byte(`"` + k.Encode() + `"`), nil
}

func (k *Key) UnmarshalJSON(d []byte) error {
	var s string
	if err := json.Unmarshal(d, &s); err != nil {
		return err
	}
	return k.Decode(s)
}

type Info interface {
	base.FileInfo
	Ratchet() *ratchet.Spiral
	PrivateName() (Name, error)
}

func Stat(f fs.File) (Info, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	pi, ok := fi.(Info)
	if !ok {
		return nil, fmt.Errorf("file %q doesn't contain private info", fi.Name())
	}
	return pi, nil
}

type privateNode interface {
	base.Node

	INumber() INumber
	Ratchet() *ratchet.Spiral
	PrivateName() (Name, error)
	BareNamefilter() BareNamefilter
	Update(content fs.File) (PutResult, error)
}

type privateTree interface {
	privateNode
	base.Tree
}

type Root struct {
	*Tree
	ctx context.Context
}

var (
	_ privateTree    = (*Root)(nil)
	_ fs.File        = (*Root)(nil)
	_ fs.ReadDirFile = (*Root)(nil)
)

func NewEmptyRoot(ctx context.Context, store Store, name string, rootKey Key) (*Root, error) {
	private, err := NewEmptyTree(store, IdentityBareNamefilter(), name)
	if err != nil {
		return nil, err
	}
	return &Root{
		ctx:  ctx,
		Tree: private,
	}, nil
}

func LoadRoot(ctx context.Context, store Store, name string, rootKey Key, rootName Name) (*Root, error) {
	if rootName == Name("") {
		return nil, fmt.Errorf("privateName is required")
	}

	data := CborByteArray{}
	exists, err := store.HAMT().Root().Find(ctx, string(rootName), &data)
	if err != nil {
		log.Debugw("LoadRoot find root name in HAMT", "name", string(rootName), "err", err)
		return nil, fmt.Errorf("opening private root: %w", err)
	} else if !exists {
		err := fmt.Errorf("finding key %s: %w", string(rootName), base.ErrNotFound)
		log.Debugw("LoadRoot", "name", string(rootName), "err", err)
		return nil, err
	}
	_, privateRoot, err := cid.CidFromBytes([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("reading CID bytes: %w", err)
	}

	tree, err := LoadTree(store, name, rootKey, privateRoot)
	if err != nil {
		return nil, err
	}
	return &Root{
		ctx:  ctx,
		Tree: tree,
	}, nil
}

func (r *Root) Context() context.Context { return r.ctx }
func (r *Root) Cid() cid.Cid {
	if r.fs.HAMT() == nil {
		return cid.Undef
	}
	return r.fs.HAMT().CID()
}
func (r *Root) HAMTCid() *cid.Cid {
	id := r.Cid()
	return &id
}

func (r *Root) Open(pathStr string) (fs.File, error) {
	path, err := base.NewPath(pathStr)
	if err != nil {
		return nil, err
	}

	return r.Get(path)
}

func (r *Root) Add(path base.Path, f fs.File) (res base.PutResult, err error) {
	res, err = r.Tree.Add(path, f)
	if err != nil {
		return nil, err
	}
	return res, r.putRoot()
}

func (r *Root) Copy(path base.Path, srcPathStr string, srcFS fs.FS) (res base.PutResult, err error) {
	res, err = r.Tree.Copy(path, srcPathStr, srcFS)
	if err != nil {
		return nil, err
	}
	return res, r.putRoot()
}

func (r *Root) Rm(path base.Path) (base.PutResult, error) {
	res, err := r.Tree.Rm(path)
	if err != nil {
		return nil, err
	}
	return res, r.putRoot()
}

func (r *Root) Mkdir(path base.Path) (res base.PutResult, err error) {
	res, err = r.Tree.Mkdir(path)
	if err != nil {
		return nil, err
	}
	return res, r.putRoot()
}

func (r *Root) Put() (base.PutResult, error) {
	ctx := context.TODO()
	log.Debugw("Root.Put", "name", r.name, "hamtCID", r.fs.HAMT().CID(), "key", Key(r.ratchet.Key()).Encode())

	// TODO(b5): note entirely sure this is necessary
	if _, err := r.fs.RatchetStore().PutRatchet(ctx, r.header.Info.INumber.Encode(), r.ratchet); err != nil {
		return nil, err
	}

	res, err := r.Tree.Put()
	if err != nil {
		return nil, err
	}
	return res, r.putRoot()
}

func (r *Root) putRoot() error {
	ctx := context.TODO()
	if r.fs.HAMT() != nil {
		if err := r.fs.HAMT().Write(ctx); err != nil {
			return err
		}
	}
	pn, err := r.PrivateName()
	if err != nil {
		return err
	}
	log.Debugw("putRoot", "privateName", string(pn), "name", r.name, "hamtCID", r.fs.HAMT().CID(), "key", Key(r.ratchet.Key()).Encode())
	return r.fs.RatchetStore().Flush()
}

type Tree struct {
	fs   Store
	name string  // not stored on the node. used to satisfy fs.File interface
	cid  cid.Cid // header node cid this tree was loaded from. empty if unstored

	header  Header
	ratchet *ratchet.Spiral
	links   PrivateLinks
}

var (
	_ privateTree    = (*Tree)(nil)
	_ Info           = (*Tree)(nil)
	_ fs.File        = (*Tree)(nil)
	_ fs.ReadDirFile = (*Tree)(nil)
)

func NewEmptyTree(fs Store, parent BareNamefilter, name string) (*Tree, error) {
	in := NewINumber()
	bnf, err := NewBareNamefilter(parent, in)
	if err != nil {
		return nil, err
	}

	return &Tree{
		fs:      fs,
		ratchet: ratchet.NewSpiral(),
		name:    name,
		header: Header{
			Info: NewHeaderInfo(base.NTDir, in, bnf),
		},
		links: PrivateLinks{},
	}, nil
}

func LoadTree(fs Store, name string, key Key, id cid.Cid) (*Tree, error) {
	log.Debugw("LoadTree", "name", name, "cid", id)
	ctx := context.TODO()

	header, err := loadHeader(ctx, fs, key, id)
	if err != nil {
		return nil, err
	}

	ratchet, err := ratchet.DecodeSpiral(header.Info.Ratchet)
	if err != nil {
		return nil, fmt.Errorf("decoding ratchet: %w", err)
	}
	header.Info.Ratchet = ""

	return &Tree{
		fs:      fs,
		name:    name,
		ratchet: ratchet,
		cid:     id,
		header:  header,
	}, nil
}

func LoadTreeFromName(ctx context.Context, fs Store, key Key, name string, pn Name) (*Tree, error) {
	id, err := cidFromPrivateName(ctx, fs, pn)
	if err != nil {
		return nil, err
	}
	return LoadTree(fs, name, key, id)
}

func (pt *Tree) Name() string                   { return pt.name }
func (pt *Tree) Size() int64                    { return pt.header.Info.Size }
func (pt *Tree) ModTime() time.Time             { return time.Unix(pt.header.Info.Mtime, 0) }
func (pt *Tree) Mode() fs.FileMode              { return fs.FileMode(pt.header.Info.Ctime) }
func (pt *Tree) Type() base.NodeType            { return pt.header.Info.Type }
func (pt *Tree) IsDir() bool                    { return true }
func (pt *Tree) Sys() interface{}               { return pt.fs }
func (pt *Tree) Stat() (fs.FileInfo, error)     { return pt, nil }
func (pt *Tree) Cid() cid.Cid                   { return pt.cid }
func (pt *Tree) INumber() INumber               { return pt.header.Info.INumber }
func (pt *Tree) Ratchet() *ratchet.Spiral       { return pt.ratchet }
func (pt *Tree) BareNamefilter() BareNamefilter { return pt.header.Info.BareNamefilter }
func (pt *Tree) PrivateFS() Store               { return pt.fs }
func (pt *Tree) AsHistoryEntry() base.HistoryEntry {
	n, _ := pt.PrivateName()
	return base.HistoryEntry{
		Cid:         pt.cid,
		Size:        pt.header.Info.Size,
		Mtime:       pt.header.Info.Mtime,
		Type:        pt.header.Info.Type,
		Key:         pt.Key().Encode(),
		PrivateName: string(n),
		// TODO(b5):
		// Previous: prevCID(pt.fs, pt.ratchet, pt.header.Info.BareNamefilter),
		// Metadata: pt.info.Metadata,
		// Size:     pt.info.Size,
	}
}

func (pt *Tree) Meta() (base.LinkedDataFile, error) {
	return nil, fmt.Errorf("unfinished: private.Tree.Meta()")
}

func (pt *Tree) ensureLinks(ctx context.Context) error {
	if pt.links == nil {
		blk, err := pt.fs.Blockservice().GetBlock(ctx, pt.header.ContentID)
		if err != nil {
			return err
		}

		pt.links, err = unmarshalPrivateLinksBlock(blk, pt.Key())
		return err
	}
	return nil
}

func (pt *Tree) PrivateName() (Name, error) {
	knf, err := AddKey(pt.header.Info.BareNamefilter, Key(pt.ratchet.Key()))
	if err != nil {
		return "", err
	}
	return ToName(knf)
}
func (pt *Tree) Key() Key { return pt.ratchet.Key() }

func (pt *Tree) Read(p []byte) (n int, err error) {
	return -1, fmt.Errorf("cannot read directory")
}
func (pt *Tree) Close() error { return nil }

func (pt *Tree) ReadDir(n int) ([]fs.DirEntry, error) {
	if err := pt.ensureLinks(context.TODO()); err != nil {
		return nil, err
	}

	if n < 0 {
		n = len(pt.links)
	}

	entries := make([]fs.DirEntry, 0, n)
	for i, link := range pt.links.SortedSlice() {
		entries = append(entries, base.NewFSDirEntry(link.Name, link.IsFile))

		if i == n {
			break
		}
	}
	return entries, nil
}

func (pt *Tree) Update(file fs.File) (PutResult, error) {
	return PutResult{}, fmt.Errorf("directories don't support updating")
}

func (pt *Tree) Add(path base.Path, f fs.File) (res base.PutResult, err error) {
	ctx := context.TODO()
	log.Debugw("Tree.Add", "path", path)
	if len(path) == 0 {
		return res, errors.New("invalid path: empty")
	}
	if err := pt.ensureLinks(context.TODO()); err != nil {
		return res, err
	}

	head, tail := path.Shift()
	if tail == nil {
		res, err = pt.createOrUpdateChildFile(ctx, head, f)
		if err != nil {
			return res, err
		}
	} else {
		childDir, err := pt.getOrCreateDirectChildTree(head)
		if err != nil {
			return res, err
		}

		// recurse
		res, err = childDir.Add(tail, f)
		if err != nil {
			return res, err
		}
	}

	pt.updateUserlandLink(head, res)
	// contents of tree have changed, write an update.
	return pt.Put()
}

func (pt *Tree) Copy(path base.Path, srcPathStr string, srcFS fs.FS) (res base.PutResult, err error) {
	log.Debugw("Tree.copy", "path", path, "srcPath", srcPathStr)
	if len(path) == 0 {
		return res, errors.New("invalid path: empty")
	}

	head, tail := path.Shift()
	if tail == nil {
		f, err := srcFS.Open(srcPathStr)
		if err != nil {
			return nil, err
		}

		res, err = pt.createOrUpdateChild(srcPathStr, head, f, srcFS)
		if err != nil {
			return res, err
		}
	} else {
		childDir, err := pt.getOrCreateDirectChildTree(head)
		if err != nil {
			return res, err
		}

		// recurse
		res, err = childDir.Copy(tail, srcPathStr, srcFS)
		if err != nil {
			return res, err
		}
	}

	pt.updateUserlandLink(head, res)
	// contents of tree have changed, write an update.
	return pt.Put()
}

func (pt *Tree) Get(path base.Path) (fs.File, error) {
	head, tail := path.Shift()
	if head == "" {
		return pt, nil
	}

	if err := pt.ensureLinks(context.TODO()); err != nil {
		return nil, err
	}

	link := pt.links.Get(head)
	if link == nil {
		return nil, base.ErrNotFound
	}

	if tail != nil {
		ch, err := LoadTree(pt.fs, head, link.Key, link.Cid)
		if err != nil {
			return nil, err
		}

		// recurse
		return ch.Get(tail)
	}

	return LoadNode(context.TODO(), pt.fs, head, link.Cid, link.Key)
}

func (pt *Tree) Rm(path base.Path) (base.PutResult, error) {
	head, tail := path.Shift()
	if head == "" {
		return nil, fmt.Errorf("invalid path: empty")
	}
	if err := pt.ensureLinks(context.TODO()); err != nil {
		return nil, err
	}

	if tail == nil {
		pt.removeUserlandLink(head)
	} else {
		link := pt.links.Get(head)
		if link == nil {
			return nil, base.ErrNotFound
		}
		child, err := LoadTree(pt.fs, link.Name, link.Key, link.Cid)
		if err != nil {
			return nil, err
		}

		// recurse
		res, err := child.Rm(tail)
		if err != nil {
			return nil, err
		}
		pt.updateUserlandLink(head, res)
	}

	// contents of tree have changed, write an update.
	return pt.Put()
}

func (pt *Tree) Mkdir(path base.Path) (res base.PutResult, err error) {
	if len(path) < 1 {
		return res, errors.New("invalid path: empty")
	}

	head, tail := path.Shift()
	childDir, err := pt.getOrCreateDirectChildTree(head)
	if err != nil {
		return nil, err
	}

	if tail == nil {
		res, err = childDir.Put()
		if err != nil {
			return nil, err
		}
	} else {
		res, err = pt.Mkdir(tail)
		if err != nil {
			return nil, err
		}
	}

	pt.updateUserlandLink(head, res)
	return pt.Put()
}

func (pt *Tree) History(ctx context.Context, maxRevs int) ([]base.HistoryEntry, error) {
	return history(ctx, pt, maxRevs)
}

func history(ctx context.Context, n privateNode, maxRevs int) ([]base.HistoryEntry, error) {
	st, err := n.Stat()
	if err != nil {
		return nil, err
	}

	bnf := n.BareNamefilter()
	store, err := NodeStore(n)
	if err != nil {
		return nil, err
	}

	old, err := store.RatchetStore().OldestKnownRatchet(ctx, n.INumber().Encode())
	if err != nil {
		log.Debugw("getting oldest known ratchet", "err", err)
		return nil, err
	}
	if old == nil {
		log.Debugw("getting oldest known ratchet", "err", err)
		return nil, err
	}

	recent := n.Ratchet()
	ratchets, err := recent.Previous(old, maxRevs)
	if err != nil {
		log.Debugw("history previous revs", "err", err)
		return nil, err
	}
	ratchets = append([]*ratchet.Spiral{recent}, ratchets...) // add current revision to top of stack

	log.Debugw("History", "name", st.Name(), "len(ratchets)", len(ratchets), "oldest_ratchet", old.Encode())

	hist := make([]base.HistoryEntry, len(ratchets))
	for i, rcht := range ratchets {
		key := Key(rcht.Key())
		knf, err := AddKey(bnf, key)
		if err != nil {
			return nil, err
		}
		pn, err := ToName(knf)
		if err != nil {
			return nil, err
		}
		headerID, err := cidFromPrivateName(ctx, store, pn)
		if err != nil {
			log.Debugw("getting CID from private name", "err", err)
			return nil, err
		}

		header, err := loadHeader(ctx, store, key, headerID)
		if err != nil {
			log.Debugw("loading historical header", "cid", headerID, "err", err)
		}

		hist[i] = base.HistoryEntry{
			Cid:   headerID,
			Size:  header.Info.Size,
			Type:  header.Info.Type,
			Mtime: header.Info.Mtime,

			Key:         key.Encode(),
			PrivateName: string(pn),
		}
	}

	log.Debugw("found history", "len(hist)", len(hist))
	return hist, nil
}

func (pt *Tree) getOrCreateDirectChildTree(name string) (*Tree, error) {
	if err := pt.ensureLinks(context.TODO()); err != nil {
		return nil, err
	}
	link := pt.links.Get(name)
	if link == nil {
		return NewEmptyTree(pt.fs, pt.header.Info.BareNamefilter, name)
	}

	return LoadTree(pt.fs, name, link.Key, link.Cid)
}

func (pt *Tree) createOrUpdateChild(srcPathStr, name string, f fs.File, srcFS fs.FS) (base.PutResult, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return pt.createOrUpdateChildDirectory(srcPathStr, name, f, srcFS)
	}
	return pt.createOrUpdateChildFile(context.TODO(), name, f)
}

func (pt *Tree) createOrUpdateChildDirectory(srcPathStr, name string, f fs.File, srcFS fs.FS) (base.PutResult, error) {
	dir, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("cannot read directory contents")
	}
	ents, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("reading directory contents: %w", err)
	}
	if err := pt.ensureLinks(context.TODO()); err != nil {
		return nil, err
	}

	var tree *Tree
	if link := pt.links.Get(name); link != nil {
		tree, err = LoadTree(pt.fs, link.Name, link.Key, link.Cid)
		if err != nil {
			return nil, err
		}
	} else {
		tree, err = NewEmptyTree(pt.fs, pt.header.Info.BareNamefilter, name)
		if err != nil {
			return nil, err
		}
	}

	var res base.PutResult
	for _, ent := range ents {
		res, err = tree.Copy(base.Path{ent.Name()}, filepath.Join(srcPathStr, ent.Name()), srcFS)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (pt *Tree) createOrUpdateChildFile(ctx context.Context, name string, f fs.File) (base.PutResult, error) {
	if err := pt.ensureLinks(ctx); err != nil {
		return nil, err
	}
	if link := pt.links.Get(name); link != nil {
		prev, err := LoadNode(ctx, pt.fs, link.Name, link.Cid, link.Key)
		if err != nil {
			log.Debugw("createOrUpdateChildFile", "err", err)
			return nil, err
		}
		return prev.Update(f)
	}

	if dataFile, ok := f.(base.LinkedDataFile); ok {
		v, err := dataFile.Data()
		if err != nil {
			return nil, err
		}
		df, err := NewDataFile(pt.fs, name, v, pt.header.Info.BareNamefilter)
		if err != nil {
			return nil, err
		}
		return df.Put()
	}

	ch, err := NewFile(pt.fs, pt.header.Info.BareNamefilter, f)
	if err != nil {
		return nil, err
	}
	return ch.Put()
}

func (pt *Tree) Put() (base.PutResult, error) {
	ctx := context.TODO()
	pt.ratchet.Inc()
	log.Debugw("Tree.Put", "name", pt.name, "len(links)", len(pt.links), "newRatchet", pt.ratchet.Summary())
	key := pt.ratchet.Key()
	pt.header.Info.Ratchet = pt.ratchet.Encode()
	pt.header.Info.Size = pt.links.SizeSum()

	linksBlk, err := pt.links.marshalEncryptedBlock(key)
	if err != nil {
		return nil, err
	}
	pt.header.ContentID = linksBlk.Cid()

	blk, err := pt.header.encryptHeaderBlock(key)
	if err != nil {
		return nil, err
	}

	if err = pt.fs.Blockservice().Blockstore().PutMany([]blocks.Block{blk, linksBlk}); err != nil {
		return nil, err
	}
	pt.cid = blk.Cid()

	privName, err := pt.PrivateName()
	if err != nil {
		return nil, err
	}

	if _, err = pt.fs.RatchetStore().PutRatchet(ctx, pt.header.Info.INumber.Encode(), pt.ratchet); err != nil {
		return nil, err
	}

	idBytes := CborByteArray(pt.cid.Bytes())
	if err := pt.fs.HAMT().Root().Set(ctx, string(privName), &idBytes); err != nil {
		return nil, err
	}

	log.Debugw("Tree.Put", "name", pt.name, "privateName", string(privName), "cid", pt.cid.String(), "size", pt.header.Info.Size)
	return PutResult{
		PutResult: public.PutResult{
			Cid:  pt.cid,
			Size: pt.header.Info.Size,
			Type: pt.header.Info.Type,
		},
		Key:     key,
		Pointer: privName,
	}, nil
}

func (pt *Tree) updateUserlandLink(name string, res base.PutResult) {
	pt.links.Add(res.(PutResult).ToPrivateLink(name))
	pt.header.Info.Mtime = base.Timestamp().Unix()
}

func (pt *Tree) removeUserlandLink(name string) {
	pt.links.Remove(name)
	pt.header.Info.Mtime = base.Timestamp().Unix()
}

type File struct {
	fs     Store
	name   string  // not persisted. used to implement fs.File interface
	cid    cid.Cid // cid header was loaded from. empty if new
	header Header

	ratchet *ratchet.Spiral
	content io.ReadCloser
}

var (
	_ privateNode = (*File)(nil)
	_ fs.File     = (*File)(nil)
	_ Info        = (*File)(nil)
)

func NewFile(fs Store, parent BareNamefilter, f fs.File) (*File, error) {
	in := NewINumber()
	bnf, err := NewBareNamefilter(parent, in)
	if err != nil {
		return nil, err
	}

	return &File{
		fs:      fs,
		ratchet: ratchet.NewSpiral(),
		content: f,
		header: Header{
			Info: NewHeaderInfo(base.NTFile, in, bnf),
		},
	}, nil
}

func LoadFile(ctx context.Context, store Store, name string, key Key, id cid.Cid) (*File, error) {
	log.Debugw("LoadFile", "name", name, "cid", id, "key", key.Encode())
	header, err := loadHeader(ctx, store, key, id)
	if err != nil {
		log.Debugw("LoadFile", "err", err)
		return nil, fmt.Errorf("decoding s-node %q header: %w", name, err)
	}

	ratchet, err := ratchet.DecodeSpiral(header.Info.Ratchet)
	if err != nil {
		return nil, err
	}
	header.Info.Ratchet = ""

	return &File{
		fs:      store,
		ratchet: ratchet,
		name:    name,
		cid:     id,
		header:  header,
	}, nil
}

func (pf *File) Ratchet() *ratchet.Spiral       { return pf.ratchet }
func (pf *File) BareNamefilter() BareNamefilter { return pf.header.Info.BareNamefilter }
func (pf *File) INumber() INumber               { return pf.header.Info.INumber }
func (pf *File) Cid() cid.Cid                   { return pf.cid }
func (pf *File) Content() cid.Cid               { return pf.header.ContentID }
func (pf *File) PrivateFS() Store               { return pf.fs }
func (pf *File) IsDir() bool                    { return false }
func (pf *File) ModTime() time.Time             { return time.Unix(pf.header.Info.Mtime, 0) }
func (pf *File) Mode() fs.FileMode              { return fs.FileMode(pf.header.Info.Mode) }
func (pf *File) Type() base.NodeType            { return pf.header.Info.Type }
func (pf *File) Name() string                   { return pf.name }
func (pf *File) Size() int64                    { return pf.header.Info.Size }
func (pf *File) Sys() interface{}               { return pf.fs }
func (pf *File) Stat() (fs.FileInfo, error)     { return pf, nil }

func (pf *File) Meta() (base.LinkedDataFile, error) {
	return nil, fmt.Errorf("unfinished: private.File.Meta()")
}

func (pf *File) PrivateName() (Name, error) {
	knf, err := AddKey(pf.header.Info.BareNamefilter, Key(pf.ratchet.Key()))
	if err != nil {
		return "", err
	}
	return ToName(knf)
}

func (pf *File) AsHistoryEntry() base.HistoryEntry {
	return base.HistoryEntry{
		// TODO(b5): finish
	}
}

func (pf *File) Key() Key { return pf.ratchet.Key() }

func (pf *File) Read(p []byte) (n int, err error) {
	if err = pf.ensureContent(); err != nil {
		return 0, err
	}
	return pf.content.Read(p)
}

func (pf *File) Close() error {
	if pf.content == nil {
		return nil
	}
	return pf.content.Close()
}

func (pf *File) History(ctx context.Context, maxRevs int) ([]base.HistoryEntry, error) {
	return history(ctx, pf, maxRevs)
}

func (pf *File) SetContents(f fs.File) {
	pf.content = f
}

func (pf *File) ensureContent() (err error) {
	if pf.content == nil {
		key := pf.ratchet.Key()
		pf.content, err = pf.fs.GetEncryptedFile(pf.header.ContentID, key[:])
		log.Debugw("opening file contents", "name", pf.name, "cid", pf.cid, "err", err)
	}
	return err
}

func (pf *File) Update(change fs.File) (result PutResult, err error) {
	if changeDF, ok := change.(base.LinkedDataFile); ok {
		v, err := changeDF.Data()
		if err != nil {
			return result, err
		}

		// update is changing from file to data file
		df := &DataFile{
			fs:   pf.fs,
			cid:  pf.cid,
			name: pf.name,
			header: Header{
				Info:     pf.header.Info.Copy(),
				Metadata: pf.header.Metadata,
			},
		}
		df.SetContents(v)
		df.header.Info.Type = base.NTDataFile
		return df.Put()
	}

	pf.SetContents(change)
	return pf.Put()
}

func (pf *File) Put() (PutResult, error) {
	ctx := context.TODO()
	store := pf.fs

	// generate a new version key by advancing the ratchet
	// TODO(b5): what happens if anything errors after advancing the ratchet?
	// assuming we need to make a point of throwing away the file & cleaning the HAMT
	pf.ratchet.Inc()
	key := pf.ratchet.Key()

	res, err := store.PutEncryptedFile(base.NewMemfileReader(pf.name, pf.content), key[:])
	if err != nil {
		return PutResult{}, err
	}

	// update header details
	pf.header.ContentID = res.Cid
	pf.header.Info.Size = res.Size
	pf.header.Info.Ratchet = pf.ratchet.Encode()
	pf.header.Info.Mtime = base.Timestamp().Unix()

	blk, err := pf.header.encryptHeaderBlock(key)
	if err != nil {
		return PutResult{}, err
	}

	if err := store.Blockservice().Blockstore().Put(blk); err != nil {
		return PutResult{}, err
	}
	pf.cid = blk.Cid()

	// create private name from key
	privName, err := pf.PrivateName()
	if err != nil {
		return PutResult{}, err
	}

	if _, err = store.RatchetStore().PutRatchet(ctx, pf.header.Info.INumber.Encode(), pf.ratchet); err != nil {
		return PutResult{}, err
	}

	idBytes := CborByteArray(pf.cid.Bytes())
	if err := pf.fs.HAMT().Root().Set(ctx, string(privName), &idBytes); err != nil {
		return PutResult{}, err
	}

	log.Debugw("File.Put", "name", pf.name, "cid", pf.cid.String(), "size", res.Size)
	return PutResult{
		PutResult: public.PutResult{
			Cid:      pf.cid,
			Type:     pf.header.Info.Type,
			Userland: res.Cid,
			Size:     res.Size,
		},
		Key:     key,
		Pointer: privName,
	}, nil
}

type INumber [32]byte

func NewINumber() INumber {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	id := [32]byte{}
	for i, v := range buf {
		id[i] = v
	}
	return id
}

func (n INumber) Encode() string { return base64.URLEncoding.EncodeToString(n[:]) }

type PrivateLink struct {
	base.Link
	Key     Key
	Pointer Name
}

func LoadNode(ctx context.Context, fs Store, name string, id cid.Cid, key Key) (privateNode, error) {
	log.Debugw("LoadNode", "name", name, "id", id)
	header, err := loadHeader(ctx, fs, key, id)
	if err != nil {
		log.Debugw("LoadNode", "err", err)
		return nil, fmt.Errorf("decoding s-node %q header: %w", name, err)
	}

	r, err := ratchet.DecodeSpiral(header.Info.Ratchet)
	if err != nil {
		return nil, err
	}
	header.Info.Ratchet = ""

	switch header.Info.Type {
	case base.NTFile:
		return &File{
			fs:      fs,
			cid:     id,
			name:    name,
			header:  header,
			ratchet: r,
		}, nil
	case base.NTDataFile:
		return &DataFile{
			fs:      fs,
			cid:     id,
			name:    name,
			header:  header,
			ratchet: r,
			content: header.Value,
		}, nil
	case base.NTDir:
		return &Tree{
			fs:      fs,
			cid:     id,
			name:    name,
			header:  header,
			ratchet: r,
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized private node type %s for cid %s", header.Info.Type, id)
	}
}

type PrivateLinks map[string]PrivateLink

func unmarshalPrivateLinksBlock(blk blocks.Block, key Key) (PrivateLinks, error) {
	aead, err := newCipher(key[:])
	if err != nil {
		return nil, err
	}
	ciphertext := blk.RawData()
	plaintext, err := aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], nil)
	if err != nil {
		return nil, err
	}

	links := PrivateLinks{}
	err = cbor.Unmarshal(plaintext, &links)
	return links, err
}

func (pls PrivateLinks) Get(name string) *PrivateLink {
	l, ok := pls[name]
	if !ok {
		return nil
	}
	return &l
}

func (pls PrivateLinks) Add(link PrivateLink) {
	pls[link.Name] = link
}

func (pls PrivateLinks) Remove(name string) bool {
	_, existed := pls[name]
	delete(pls, name)
	return existed
}

func (pls PrivateLinks) SortedSlice() []PrivateLink {
	names := make([]string, 0, len(pls))
	for name := range pls {
		names = append(names, name)
	}
	sort.Strings(names)

	links := make([]PrivateLink, 0, len(pls))
	for _, name := range names {
		links = append(links, pls[name])
	}
	return links
}

func (pls PrivateLinks) SizeSum() (total int64) {
	for _, l := range pls {
		total += l.Size
	}
	return total
}

func (pls PrivateLinks) marshalEncryptedBlock(key Key) (blocks.Block, error) {
	plaintext, err := cbor.Marshal(pls)
	if err != nil {
		return nil, err
	}

	log.Debugw("encrypting private links", "key", key.Encode())
	aead, err := newCipher(key[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	// TODO(b5): still using random nonces, switching to monotonic long-term
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)
	data := append(nonce, ciphertext...)

	hash, err := multihash.Sum(data, base.DefaultMultihashType, -1)

	return blocks.NewBlockWithCid(data, cid.NewCidV1(cid.Raw, hash))
}

type PutResult struct {
	public.PutResult
	Key     Key
	Pointer Name
}

func (r PutResult) ToPrivateLink(name string) PrivateLink {
	return PrivateLink{
		Link:    r.ToLink(name),
		Key:     r.Key,
		Pointer: r.Pointer,
	}
}

func cidFromPrivateName(ctx context.Context, fs Store, pn Name) (id cid.Cid, err error) {
	exists, data, err := fs.HAMT().Root().FindRaw(ctx, string(pn))
	if err != nil {
		return id, err
	}
	if !exists {
		return id, base.ErrNotFound
	}

	// TODO(b5): lol wtf just plugged this 2 byte prefix strip in & CID parsing works,
	// figure out the proper way to decode cids out of the HAMT
	_, id, err = cid.CidFromBytes(data[2:])
	return id, err
}

type Header struct {
	Info      HeaderInfo
	Metadata  cid.Cid
	ContentID cid.Cid
	Value     interface{} // only present on dataFile nodes
}

type HeaderInfo struct {
	WNFS  base.SemVer
	Type  base.NodeType
	Mode  uint32
	Ctime int64
	Mtime int64
	Size  int64

	INumber        INumber
	BareNamefilter BareNamefilter
	Ratchet        string
}

func NewHeaderInfo(nt base.NodeType, in INumber, bnf BareNamefilter) HeaderInfo {
	now := base.Timestamp().Unix()
	return HeaderInfo{
		WNFS:  base.LatestVersion,
		Type:  nt,
		Mode:  base.ModeDefault,
		Ctime: now,
		Mtime: now,
		Size:  0,

		INumber:        in,
		BareNamefilter: bnf,
	}
}

func HeaderInfoFromCBOR(d []byte) (HeaderInfo, error) {
	hi := HeaderInfo{}
	err := base.DecodeCBOR(d, &hi)
	return hi, err
}

func (hi HeaderInfo) CBOR() (*bytes.Buffer, error) {
	return base.EncodeCBOR(hi)
}

func (hi HeaderInfo) Copy() HeaderInfo {
	return HeaderInfo{
		WNFS:           hi.WNFS,
		Type:           hi.Type,
		Mode:           hi.Mode,
		Ctime:          hi.Ctime,
		Mtime:          hi.Mtime,
		Size:           hi.Size,
		INumber:        hi.INumber,
		BareNamefilter: hi.BareNamefilter,
		Ratchet:        hi.Ratchet,
	}
}

func (h Header) encryptHeaderBlock(key Key) (blocks.Block, error) {
	buf, err := h.Info.CBOR()
	if err != nil {
		return nil, err
	}

	log.Debugw("encrypting header info block", "key", key.Encode())
	aead, err := newCipher(key[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	// TODO(b5): still using random nonces, switching to monotonic long-term
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	encrypted := aead.Seal(nil, nonce, buf.Bytes(), nil)
	header := map[string]interface{}{
		"info":    append(nonce, encrypted...),
		"content": h.ContentID,
	}
	log.Debugw("content", "cid", h.ContentID)
	if !h.Metadata.Equals(cid.Undef) {
		header["metadata"] = h.Metadata
	}
	return cbornode.WrapObject(header, base.DefaultMultihashType, -1)
}

func loadHeader(ctx context.Context, s Store, key Key, id cid.Cid) (h Header, err error) {
	log.Debugw("loadHeader", "cid", id, "key", key.Encode())
	blk, err := s.Blockservice().GetBlock(ctx, id)
	if err != nil {
		return h, fmt.Errorf("getting header block %q: %w", id.String(), err)
	}

	return decodeHeaderBlock(blk, key)
}

func decodeHeaderBlock(blk blocks.Block, key Key) (h Header, err error) {
	env := map[string]interface{}{}
	if err := cbor.Unmarshal(blk.RawData(), &env); err != nil {
		log.Debugw("decodeHeaderBlock", "err", err, "data", fmt.Sprintf("%x", blk.RawData()))
		return h, err
	}

	encInfo, ok := env["info"].([]byte)
	if !ok {
		return h, fmt.Errorf("header is missing info field")
	}

	aead, err := newCipher(key[:])
	if err != nil {
		return h, err
	}
	plaintext, err := aead.Open(nil, encInfo[:aead.NonceSize()], encInfo[aead.NonceSize():], nil)
	if err != nil {
		log.Debugw("decodeHeaderBlock info", "err", err)
		return h, fmt.Errorf("decrypting info: %w", err)
	}

	if h.Info, err = HeaderInfoFromCBOR(plaintext); err != nil {
		log.Debugw("decodeHeaderBlock", "err", err)
		return h, err
	}

	if meta, ok := env["metadata"].(cbor.Tag); ok {
		if h.Metadata, err = cidFromCBORTag(meta); err != nil {
			log.Debugw("decodeHeaderBlock", "err", err)
			return h, err
		}
	}

	if h.Info.Type == base.NTDataFile {
		// TODO(b5): this is probably the right place to decode content
		if encValue, ok := env["value"].([]byte); ok {
			plaintext, err = aead.Open(nil, encValue[:aead.NonceSize()], encValue[aead.NonceSize():], nil)
			if err != nil {
				log.Debugw("decodeHeaderBlock value", "err", err)
				return h, err
			}
			var v interface{}
			if err = cbornode.DecodeInto(plaintext, &v); err != nil {
				return h, err
			}
			h.Value = v
		} else {
			return h, fmt.Errorf("datafile header has no value field")
		}
	} else {
		if content, ok := env["content"].(cbor.Tag); ok {
			if h.ContentID, err = cidFromCBORTag(content); err != nil {
				log.Debugw("decodeHeaderBlock", "err", err)
				return h, err
			}
		} else {
			return h, fmt.Errorf("header has no content cid")
		}
	}

	return h, nil
}

func cidFromCBORTag(v interface{}) (cid.Cid, error) {
	t, ok := v.(cbor.Tag)
	if !ok {
		return cid.Undef, fmt.Errorf("expected value to be a cbor.Tag")
	}
	d, ok := t.Content.([]byte)
	if !ok {
		return cid.Undef, fmt.Errorf("expected tag contents to be bytes")
	}
	return cid.Cast(d[1:])
}

type DataFile struct {
	fs   Store
	name string
	cid  cid.Cid

	ratchet     *ratchet.Spiral
	header      Header
	Metadata    *cid.Cid
	content     interface{}
	jsonContent *bytes.Buffer
}

var (
	_ base.LinkedDataFile = (*DataFile)(nil)
	_ base.Node           = (*DataFile)(nil)
)

func NewDataFile(fs Store, name string, content interface{}, parent BareNamefilter) (*DataFile, error) {
	in := NewINumber()
	bnf, err := NewBareNamefilter(parent, in)
	if err != nil {
		return nil, err
	}

	return &DataFile{
		fs:      fs,
		name:    name,
		ratchet: ratchet.NewSpiral(),
		header: Header{
			Info: NewHeaderInfo(base.NTDataFile, in, bnf),
		},
		content: content,
	}, nil
}

func LoadDataFile(ctx context.Context, fs Store, name string, id cid.Cid, key Key) (*DataFile, error) {
	df := &DataFile{
		fs:   fs,
		name: name,
		cid:  id,
	}

	blk, err := fs.Blockservice().GetBlock(ctx, id)
	if err != nil {
		return nil, err
	}

	return decodeDataFileBlock(df, blk, key)
}

func decodeDataFileBlock(df *DataFile, blk blocks.Block, key Key) (*DataFile, error) {
	aead, err := newCipher(key[:])
	if err != nil {
		return nil, err
	}

	env := map[string]interface{}{}
	if err := cbornode.DecodeInto(blk.RawData(), &env); err != nil {
		return nil, err
	}

	ciphertext, ok := env["info"].([]byte)
	if !ok {
		return nil, fmt.Errorf("malformed private DataFile node %s: missing info bytes", blk.Cid())
	}
	plaintext, err := aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], nil)
	if err != nil {
		return nil, err
	}

	df.header.Info, err = HeaderInfoFromCBOR(plaintext)
	if err != nil {
		return nil, err
	}

	if ciphertext, ok = env["content"].([]byte); !ok {
		return nil, fmt.Errorf("malformed private DataFile node %s: missing content bytes", blk.Cid())
	}
	if plaintext, err = aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], nil); err != nil {
		return nil, err
	}
	var content interface{}
	if err := cbor.Unmarshal(plaintext, &content); err != nil {
		return nil, err
	}
	df.content = content

	return df, nil
}

func (df *DataFile) IsBare() bool                   { return false }
func (df *DataFile) Links() base.Links              { return base.NewLinks() } // TODO(b5): remove Links method?
func (df *DataFile) Name() string                   { return df.name }
func (df *DataFile) Size() int64                    { return df.header.Info.Size }
func (df *DataFile) ModTime() time.Time             { return time.Unix(df.header.Info.Mtime, 0) }
func (df *DataFile) Mode() fs.FileMode              { return fs.FileMode(df.header.Info.Mode) }
func (df *DataFile) Type() base.NodeType            { return df.header.Info.Type }
func (df *DataFile) IsDir() bool                    { return false }
func (df *DataFile) Sys() interface{}               { return df.fs }
func (df *DataFile) Cid() cid.Cid                   { return df.cid }
func (df *DataFile) Stat() (fs.FileInfo, error)     { return df, nil }
func (df *DataFile) Data() (interface{}, error)     { return df.content, nil }
func (df *DataFile) BareNamefilter() BareNamefilter { return df.header.Info.BareNamefilter }
func (df *DataFile) INumber() INumber               { return df.header.Info.INumber }
func (df *DataFile) Ratchet() *ratchet.Spiral       { return df.ratchet }
func (df *DataFile) PrivateName() (Name, error) {
	knf, err := AddKey(df.header.Info.BareNamefilter, Key(df.ratchet.Key()))
	if err != nil {
		return "", err
	}
	return ToName(knf)
}

func (df *DataFile) Meta() (base.LinkedDataFile, error) {
	return nil, fmt.Errorf("unfinished: private.DataFile.Meta")
}

func (df *DataFile) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, fmt.Errorf("unfinished: private.DataFile.ReadDir")
}

func (df *DataFile) History(ctx context.Context, maxRevs int) ([]base.HistoryEntry, error) {
	// TODO(b5): support history
	return nil, fmt.Errorf("no history")
}

func (df *DataFile) Read(p []byte) (n int, err error) {
	if err = df.ensureContent(); err != nil {
		return 0, err
	}
	return df.jsonContent.Read(p)
}

func (df *DataFile) ensureContent() (err error) {
	if df.jsonContent == nil {
		log.Debugw("DataFile loading content", "name", df.name, "cid", df.cid)
		buf := &bytes.Buffer{}
		// TODO(b5): use faster json lib
		if err := json.NewEncoder(buf).Encode(df.content); err != nil {
			return err
		}
		df.jsonContent = buf
	}
	return nil
}

func (df *DataFile) Close() error { return nil }

func (df *DataFile) SetContents(data interface{}) {
	df.content = data
	df.jsonContent = nil
}

func (df *DataFile) Update(change fs.File) (result PutResult, err error) {
	if changeDF, ok := change.(base.LinkedDataFile); ok {
		v, err := changeDF.Data()
		if err != nil {
			return result, err
		}
		df.SetContents(v)
		return df.Put()
	}

	// update is changing from data file to file
	f := &File{
		fs:   df.fs,
		name: df.name,
		cid:  df.cid,
		header: Header{
			Info:     df.header.Info.Copy(),
			Metadata: df.header.Metadata,
		},
		content: change,
	}
	f.header.Info.Type = base.NTFile
	return f.Put()
}

func (df *DataFile) Put() (result PutResult, err error) {
	df.ratchet.Inc()
	key := df.ratchet.Key()

	// df.header.Info.Size = ???
	df.header.Info.Ratchet = df.ratchet.Encode()
	df.header.Info.Mtime = base.Timestamp().Unix()

	blk, err := df.encodeBlock(key)
	if err != nil {
		return result, err
	}
	df.cid = blk.Cid()

	name, err := df.PrivateName()
	if err != nil {
		return result, err
	}

	if err = df.fs.Blockservice().Blockstore().Put(blk); err != nil {
		return result, err
	}

	log.Debugw("wrote public data file", "name", df.name, "cid", df.cid.String())
	return PutResult{
		PutResult: public.PutResult{
			Cid:      df.cid,
			Size:     df.header.Info.Size,
			Userland: df.cid,
			Type:     df.header.Info.Type,
		},
		Key:     df.ratchet.Key(),
		Pointer: name,
	}, nil
}

func (df *DataFile) AsHistoryEntry() base.HistoryEntry {
	return base.HistoryEntry{
		Cid:   df.cid,
		Size:  df.header.Info.Size,
		Type:  df.header.Info.Type,
		Mtime: df.header.Info.Mtime,
	}
}

func (df *DataFile) encodeBlock(key Key) (blocks.Block, error) {
	aead, err := newCipher(key[:])
	if err != nil {
		return nil, err
	}

	data, err := cbor.Marshal(df.header.Info)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	cipher := aead.Seal(nil, nonce, data, nil)
	infoCipher := append(nonce, cipher...)

	data, err = cbor.Marshal(df.content)
	if err != nil {
		return nil, err
	}
	nonce = nonce[:]
	rand.Read(nonce)
	cipher = aead.Seal(nil, nonce, data, nil)
	contentCipher := append(nonce, cipher...)

	// TODO(b5): link name obfuscation
	dataFile := map[string]interface{}{
		"info":     infoCipher,
		"value":    contentCipher,
		"metadata": df.Metadata,
	}

	return cbornode.WrapObject(dataFile, base.DefaultMultihashType, -1)
}
