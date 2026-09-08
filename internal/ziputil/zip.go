// Package ziputil 提供目录打包为 zip 的工具。
package ziputil

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
)

// ZipDir 将 srcDir 递归打包为 dstZip（deflate，保留相对路径）。
func ZipDir(srcDir, dstZip string) error {
	if err := os.MkdirAll(filepath.Dir(dstZip), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dstZip)
	if err != nil {
		return err
	}
	w := zip.NewWriter(f)
	err = filepath.WalkDir(srcDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		out, err := w.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, src)
		src.Close()
		return err
	})
	if err != nil {
		w.Close()
		f.Close()
		return err
	}
	if err := w.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
