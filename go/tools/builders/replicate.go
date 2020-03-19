// Copyright 2018 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// stdlib builds the standard library in the appropriate mode into a new goroot.
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type replicateMode int

const (
	copyMode replicateMode = iota
	hardlinkMode
	softlinkMode
)

type replicateOption func(*replicateConfig)
type replicateConfig struct {
	removeFirst bool
	fileMode    replicateMode
	dirMode     replicateMode
	paths       []string
	zip         string
}

// replicator implementations are capable to copying a filetree from src to
// dst under the specified configuration settings
type replicator interface {
	Replicate(src, dst string, config *replicateConfig) error
}

// replicatePaths is a replicateOption that sets the configuration file paths
func replicatePaths(paths ...string) replicateOption {
	return func(config *replicateConfig) {
		config.paths = append(config.paths, paths...)
	}
}

// replicateFromZip is a replicateOption that sets the configuration zip file
// name.
func replicateFromZip(zip string) replicateOption {
	return func(config *replicateConfig) {
		config.zip = zip
	}
}

// replicatePrepare is the common preparation steps for a replication entry
func replicatePrepare(dst string, config *replicateConfig) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("Failed to make %s: %v", dir, err)
	}
	if config.removeFirst {
		_ = os.Remove(dst)
	}
	return nil
}

// createFile takes a file source reader and FileInfo, creates at file at dst,
// and updates the file mode.
func createFile(in io.Reader, stat os.FileInfo, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeerr := out.Close()
	if err != nil {
		return err
	}
	if closeerr != nil {
		return closeerr
	}
	if err := os.Chmod(dst, stat.Mode()); err != nil {
		return err
	}
	return nil
}

// replicateFile is called internally by replicate to map a single file from src into dst.
func replicateFile(src, dst string, config *replicateConfig) error {
	if err := replicatePrepare(dst, config); err != nil {
		return err
	}
	switch config.fileMode {
	case copyMode:
		s, err := os.Stat(src)
		if err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		return createFile(in, s, dst)
	case hardlinkMode:
		return os.Link(src, dst)
	case softlinkMode:
		return os.Symlink(src, dst)
	default:
		return fmt.Errorf("Invalid replication mode %d", config.fileMode)
	}
}

// replicateDir makes a tree of files visible in a new location.
// It is allowed to take any efficient method of doing so.
func replicateDir(src, dst string, config *replicateConfig) error {
	if err := replicatePrepare(dst, config); err != nil {
		return err
	}
	switch config.dirMode {
	case copyMode:
		return filepath.Walk(src, func(path string, f os.FileInfo, err error) error {
			if f.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			return replicateFile(path, filepath.Join(dst, relative), config)
		})
	case hardlinkMode:
		return os.Link(src, dst)
	case softlinkMode:
		return os.Symlink(src, dst)
	default:
		return fmt.Errorf("Invalid replication mode %d", config.fileMode)
	}
}

func replicateTree(src, dst string, config *replicateConfig) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("Failed to remove file at destination %s: %v", dst, err)
	}
	if l, err := filepath.EvalSymlinks(src); err != nil {
		return err
	} else {
		src = l
	}
	if s, err := os.Stat(src); err != nil {
		return err
	} else if s.IsDir() {
		return replicateDir(src, dst, config)
	}
	return replicateFile(src, dst, config)
}

// replicate makes a tree of files visible in a new location. You control how it
// does so using options, by default it presumes the entire tree of files rooted
// at src must be visible at dst, and that it should do so by copying. src is
// allowed to be a file, in which case just the one file is copied.
func replicate(src, dst string, options ...replicateOption) error {
	config := replicateConfig{
		removeFirst: true,
	}
	for _, option := range options {
		option(&config)
	}

	var replicator replicator
	if config.zip == "" {
		replicator = &filesystemReplicator{}
	} else {
		replicator = &zipReplicator{}
	}

	return replicator.Replicate(src, dst, &config)
}

// filesystemReplicator implements the replicator interface when source paths
// represent pre-existing entries in the filesystem.
type filesystemReplicator struct {
}

// Replicate is called for each single src dst pair.
func (r *filesystemReplicator) Replicate(src, dst string, config *replicateConfig) error {
	if len(config.paths) == 0 {
		return replicateTree(src, dst, config)
	}
	for _, base := range config.paths {
		from := filepath.Join(src, base)
		to := filepath.Join(dst, base)
		if err := replicateTree(from, to, config); err != nil {
			return err
		}
	}
	return nil
}

// zipReplicator implements the replicator interface when source paths represent
// entries in a zip archive.
type zipReplicator struct {
}

// Replicate is called for each single src dst pair.
func (r *zipReplicator) Replicate(src, dst string, config *replicateConfig) error {
	in, err := zip.OpenReader(config.zip)
	if err != nil {
		return err
	}
	defer in.Close()

	dirs := make(map[string]bool)
	files := make([]*zip.File, 0)

	// Collect all the zipfile entries of interest based on path prefixes in the
	// config.
	// TODO: construct a prefix trie here to remove this nested loop
	for _, f := range in.File {
		for _, path := range config.paths {
			if strings.HasPrefix(f.Name, path) {
				if f.FileInfo().IsDir() {
					// Although this check for IsDir is done, in practice the
					// bazel zipper utility does not create zip directory
					// entries, so in the usual case this branch is never
					// executed.
					dirs[f.Name] = true
				} else {
					files = append(files, f)
					dirs[filepath.Dir(f.Name)] = true
				}
				break
			}
		}
	}

	extract := func(file *zip.File) error {
		to := filepath.Join(dst, file.Name)
		if err := os.MkdirAll(filepath.Dir(to), os.ModePerm); err != nil {
			return err
		}
		f, err := file.Open()
		if err != nil {
			return fmt.Errorf("could not open zip file entry %s: %v", file.Name, err)
		}
		defer f.Close()

		return createFile(f, file.FileInfo(), to)
	}

	for dir := range dirs {
		to := filepath.Join(dst, dir)
		if err := replicatePrepare(to, config); err != nil {
			return err
		}
	}

	for _, f := range files {
		if err := extract(f); err != nil {
			return err
		}
	}

	return nil
}
