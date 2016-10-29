package layer

import (
	"fmt"
	"os"
	"path/filepath"
)

const dirMode = 0777

// Layer represents a filesystem layer in a container build.
type Layer struct {
	dirname    string
	workingDir string
}

// New creates a new layer.
func New(dirname string, workingDir string) (*Layer, error) {
	if dirname == "" {
		return nil, fmt.Errorf("Dirname may not be empty")
	}

	var err error

	if workingDir == "" {
		workingDir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}

	if filepath.IsAbs(dirname) {
		return nil, fmt.Errorf("Cannot use absolute path for dirname: %q", dirname)
	}

	splitList := filepath.SplitList(dirname)
	for _, name := range splitList {
		if name == ".." {
			return nil, fmt.Errorf("Cannot use .. in path names: %q", dirname)
		}
	}

	if workingDir, err = filepath.Abs(workingDir); err != nil {
		return nil, err
	}

	return &Layer{
		dirname:    dirname,
		workingDir: workingDir,
	}, nil
}

func (l *Layer) inChdir(fun func(l *Layer) error) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	if err := os.Chdir(l.workingDir); err != nil {
		return err
	}

	if err := fun(l); err != nil {
		return err
	}

	return os.Chdir(wd)
}

func (l *Layer) qualified() string {
	return filepath.Join(l.workingDir, l.dirname)
}

// Create creates the layer
func (l *Layer) Create() error {
	return l.inChdir(func(l *Layer) error { return os.Mkdir(l.qualified(), dirMode) })
}

// Remove removes the layer
func (l *Layer) Remove() error {
	return l.inChdir(func(l *Layer) error { return os.RemoveAll(l.qualified()) })
}
