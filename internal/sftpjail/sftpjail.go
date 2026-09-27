// Package sftpjail is an SFTP server confined to one directory. It backs
// local-to-remote mounts: the remote's sshfs talks to it, and can reach only
// the shared directory, even through symlinks or "..". (OpenSSH's
// sftp-server would serve everything the user can read.)
package sftpjail

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
)

// Serve answers SFTP requests on rwc for files under dir until rwc closes.
func Serve(dir string, rwc io.ReadWriteCloser) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	h := &handler{root: root}
	srv := sftp.NewRequestServer(rwc, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
	defer srv.Close()
	if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type handler struct {
	root *os.Root
}

// rel turns an SFTP path (always absolute and cleaned) into a path under the
// root. os.Root rejects anything that would leave it.
func rel(p string) string {
	if p = strings.TrimPrefix(p, "/"); p == "" {
		return "."
	}
	return p
}

// mapErr turns an escape attempt into a permission error the client
// understands.
func mapErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "escapes from parent") {
		return sftp.ErrSSHFxPermissionDenied
	}
	return err
}

func (h *handler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f, err := h.root.Open(rel(r.Filepath))
	return f, mapErr(err)
}

func (h *handler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	return h.open(r, os.O_WRONLY)
}

func (h *handler) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.open(r, os.O_RDWR)
}

func (h *handler) open(r *sftp.Request, flag int) (*os.File, error) {
	pf := r.Pflags()
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	if pf.Append {
		flag |= os.O_APPEND
	}
	perm := fs.FileMode(0o644)
	if r.AttrFlags().Permissions {
		perm = r.Attributes().FileMode().Perm()
	}
	f, err := h.root.OpenFile(rel(r.Filepath), flag, perm)
	return f, mapErr(err)
}

func (h *handler) Filecmd(r *sftp.Request) error {
	p := rel(r.Filepath)
	switch r.Method {
	case "Setstat":
		return mapErr(h.setstat(r, p))
	case "Rename":
		// SFTP v3 rename must not replace an existing file.
		if _, err := h.root.Lstat(rel(r.Target)); err == nil {
			return sftp.ErrSSHFxFailure
		}
		return mapErr(h.root.Rename(p, rel(r.Target)))
	case "Rmdir", "Remove":
		return mapErr(h.root.Remove(p))
	case "Mkdir":
		perm := fs.FileMode(0o755)
		if r.AttrFlags().Permissions {
			perm = r.Attributes().FileMode().Perm()
		}
		return mapErr(h.root.Mkdir(p, perm))
	case "Link":
		return mapErr(h.root.Link(p, rel(r.Target)))
	case "Symlink":
		// Filepath is the link's content, Target the link to create. The
		// content is just text; following it through the root stays inside.
		return mapErr(h.root.Symlink(r.Filepath, rel(r.Target)))
	}
	return sftp.ErrSSHFxOpUnsupported
}

func (h *handler) PosixRename(r *sftp.Request) error {
	return mapErr(h.root.Rename(rel(r.Filepath), rel(r.Target)))
}

func (h *handler) setstat(r *sftp.Request, p string) error {
	flags, attrs := r.AttrFlags(), r.Attributes()
	if flags.Size {
		f, err := h.root.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		err = f.Truncate(int64(attrs.Size))
		f.Close()
		if err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := h.root.Chmod(p, attrs.FileMode().Perm()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := h.root.Chtimes(p, time.Unix(int64(attrs.Atime), 0), time.Unix(int64(attrs.Mtime), 0)); err != nil {
			return err
		}
	}
	if flags.UidGid {
		if err := h.root.Chown(p, int(attrs.UID), int(attrs.GID)); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) StatVFS(r *sftp.Request) (*sftp.StatVFS, error) {
	f, err := h.root.Open(rel(r.Filepath))
	if err != nil {
		return nil, mapErr(err)
	}
	defer f.Close()
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	return &sftp.StatVFS{
		Bsize: uint64(st.Bsize), Frsize: uint64(st.Frsize),
		Blocks: st.Blocks, Bfree: st.Bfree, Bavail: st.Bavail,
		Files: st.Files, Ffree: st.Ffree, Favail: st.Ffree,
		Namemax: uint64(st.Namelen),
	}, nil
}

func (h *handler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p := rel(r.Filepath)
	switch r.Method {
	case "List":
		f, err := h.root.Open(p)
		if err != nil {
			return nil, mapErr(err)
		}
		defer f.Close()
		infos, err := f.Readdir(-1)
		return lister(infos), err
	case "Stat":
		fi, err := h.root.Stat(p)
		if err != nil {
			return nil, mapErr(err)
		}
		return lister{fi}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (h *handler) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	fi, err := h.root.Lstat(rel(r.Filepath))
	if err != nil {
		return nil, mapErr(err)
	}
	return lister{fi}, nil
}

func (h *handler) Readlink(p string) (string, error) {
	target, err := h.root.Readlink(rel(p))
	return target, mapErr(err)
}

type lister []os.FileInfo

func (l lister) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[offset:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}
