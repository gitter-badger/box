package layer

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Filesystem encapsulates a fully mounted filesystem. It is manipulated by
// adding layers and unmounting (and remounting) the product.
type Filesystem struct {
	Layers     []*Layer
	Mountpoint string
	workDir    string
}

// Mount creates any missing layers and mounts the filesystem.
func (f *Filesystem) Mount(work string) error {
	for _, layer := range f.Layers {
		if !layer.Exists() {
			if err := layer.Create(); err != nil {
				return err
			}
		}
	}

	var lower []*Layer
	var upper *Layer

	if len(f.Layers) == 1 {
		return fmt.Errorf("Minimum 2 layers for mountpoint %q: got 1", f.Mountpoint)
	}

	lower = f.Layers[:len(f.Layers)-1]
	upper = f.Layers[len(f.Layers)-1]

	if work == "" {
		return fmt.Errorf("In mount of mountpoint %q: workdir cannot be empty", f.Mountpoint)
	}

	f.workDir = work
	if err := os.Mkdir(work, 0700); err != nil {
		return err
	}

	lowerStrs := []string{}
	for _, layer := range lower {
		lowerStrs = append(lowerStrs, layer.Path())
	}

	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lowerStrs, ":"), upper.Path(), work)
	fmt.Println(data)

	return unix.Mount("overlay", f.Mountpoint, "overlay", 0, data)
}
