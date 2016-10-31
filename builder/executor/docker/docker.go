package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/pkg/term"
	"github.com/erikh/box/builder/config"
	"github.com/erikh/box/builder/executor"
	"github.com/erikh/box/log"
	"github.com/fatih/color"
)

// Docker implements an executor that talks to docker to achieve its goals.
type Docker struct {
	client   *client.Client
	config   *config.Config
	useCache bool
	tty      bool
	stdin    bool
}

// NewDocker constructs a new docker instance, for executing against docker
// engines.
func NewDocker(useCache, tty bool) (*Docker, error) {
	client, err := client.NewEnvClient()
	if err != nil {
		return nil, err
	}

	return &Docker{
		tty:      tty,
		useCache: useCache,
		client:   client,
		config:   config.NewConfig(),
	}, nil
}

// SetStdin turns on the stdin features during run invocations. It is used to
// facilitate debugging.
func (d *Docker) SetStdin(on bool) {
	d.stdin = on
}

// ImageID returns the image identifier of the most recent layer.
func (d *Docker) ImageID() string {
	return d.config.Image
}

// UseCache determines if the cache should be considered or not.
func (d *Docker) UseCache(arg bool) {
	d.useCache = arg
}

// UseTTY determines whether or not to allow docker to use a TTY for both run
// and pull operations.
func (d *Docker) UseTTY(arg bool) {
	d.tty = arg
}

// LoadConfig loads the configuration into the executor.
func (d *Docker) LoadConfig(c *config.Config) error {
	d.config = c
	return nil
}

// Config returns the current *Config for the executor.
func (d *Docker) Config() *config.Config {
	return d.config
}

// Commit commits an entry to the layer list.
func (d *Docker) Commit(cacheKey string, hook executor.Hook) error {
	id, err := d.Create()
	if err != nil {
		return err
	}

	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		_, ok := <-signals
		if ok {
			d.Destroy(id)
		}
	}()

	defer func() {
		d.Destroy(id)
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		close(signals)
	}()

	if hook != nil {
		tmp, err := hook(id)
		if err != nil {
			return err
		}

		if tmp != "" {
			cacheKey = tmp
		}
	}

	commitResp, err := d.client.ContainerCommit(context.Background(), id, types.ContainerCommitOptions{Config: d.config.ToDocker(d.tty, d.stdin), Comment: cacheKey})
	if err != nil {
		return fmt.Errorf("Error during commit: %v", err)
	}

	// try a clean remove first, otherwise the defer above will take over in a last-ditch attempt
	err = d.client.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{})
	if err != nil {
		return fmt.Errorf("Could not remove intermediate container %q: %v", id, err)
	}

	d.config.Image = commitResp.ID

	return nil
}

// CheckCache consults the cache and returns true or false depending on whether
// there was a match. If there was an error consulting the cache, it will be
// returned as the second argument.
func (d *Docker) CheckCache(cacheKey string) (bool, error) {
	if !d.useCache {
		return false, nil
	}

	if d.config.Image != "" {
		images, err := d.client.ImageList(context.Background(), types.ImageListOptions{All: true})
		if err != nil {
			return false, err
		}

		for _, img := range images {
			if img.ParentID == d.config.Image {
				inspect, _, err := d.client.ImageInspectWithRaw(context.Background(), img.ID)
				if err != nil {
					return false, err
				}

				if inspect.Comment == cacheKey {
					log.CacheHit(img.ID)
					d.config.FromDocker(inspect.Config)
					d.config.Image = img.ID
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// CopyOneFileFromContainer copies a file from the container and returns its content.
// An error is returned, if any.
func (d *Docker) CopyOneFileFromContainer(fn string) ([]byte, error) {
	id, err := d.Create()
	if err != nil {
		return nil, err
	}

	defer d.Destroy(id)

	rc, _, err := d.client.CopyFromContainer(context.Background(), id, fn)
	if err != nil {
		return nil, err
	}

	tr := tar.NewReader(rc)
	defer rc.Close()

	var header *tar.Header

	for {
		header, err = tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, err
		}

		if header.Name == filepath.Base(fn) {
			break
		}
	}

	if header == nil || header.Name != filepath.Base(fn) {
		return nil, fmt.Errorf("Could not find %q in container", fn)
	}

	return ioutil.ReadAll(tr)
}

// Create creates a new container based on the existing configuration.
func (d *Docker) Create() (string, error) {
	cont, err := d.client.ContainerCreate(
		context.Background(),
		d.config.ToDocker(d.tty, d.stdin),
		nil,
		nil,
		"",
	)

	return cont.ID, err
}

// Destroy destroys a container for the given id.
func (d *Docker) Destroy(id string) error {
	return d.client.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{Force: true})
}

// CopyFromContainer copies a series of files in a similar fashion to
// CopyToContainer, just in reverse.
func (d *Docker) CopyFromContainer(id, path string) (io.Reader, int64, error) {
	rc, stat, err := d.client.CopyFromContainer(context.Background(), id, path)
	return rc, stat.Size, err
}

// CopyToContainer copies a tarred up series of files (passed in through the
// io.Reader handle) to the container where they are untarred.
func (d *Docker) CopyToContainer(id string, size int64, tw io.Reader) error {
	tf, err := ioutil.TempFile("", "box-temporary-layer")
	if err != nil {
		return err
	}

	defer tf.Close() // second close is fine here
	defer os.Remove(tf.Name())

	if _, err := io.Copy(tf, tw); err != nil {
		return err
	}

	tf.Close()

	errChan := make(chan error)

	copyID := id
	jsonFile := fmt.Sprintf("%s.json", copyID)
	tarFile := fmt.Sprintf("%s/layer.tar", copyID)

	repos := map[string]map[string]string{
		copyID: {"latest": copyID},
	}

	manifest := []map[string]interface{}{{
		"Config":   jsonFile,
		"RepoTags": []string{copyID},
		"Layers":   []string{tarFile},
	}}

	image := d.config.ToImage([]string{copyID})

	r, w := io.Pipe()
	r2 := io.TeeReader(r, w)
	go func(r io.Reader) {
		f, err := os.Create("test")

		if err != nil {
			panic(err)
		}

		fmt.Println(io.Copy(f, r))
	}(r2)

	go func(r io.ReadCloser) {
		io.Copy(os.Stdout, r)
		rc, err := d.client.ImageImport(context.Background(), types.ImageImportSource{Source: r}, "box-"+copyID, types.ImageImportOptions{})
		if err == nil {
			// FIXME workaround for a client issue. Fix this in docker.
			content, err := ioutil.ReadAll(rc)
			if err != nil {
				errChan <- err
				return
			}

			lines := bytes.Split(content, []byte("\r\n"))
			for _, line := range lines {
				result := map[string]interface{}{}
				fmt.Println("line:", string(line))

				if err := json.Unmarshal(line, &result); err != nil {
					errChan <- err
					return
				}

				if res, ok := result["error"].(string); ok {
					errChan <- errors.New(res)
					return
				}
			}
		}

		errChan <- err
	}(r)

	imgwriter := tar.NewWriter(w)

	content, err := json.Marshal(image)
	if err != nil {
		return err
	}

	err = imgwriter.WriteHeader(&tar.Header{
		Uname:      "root",
		Gname:      "root",
		Name:       jsonFile,
		Linkname:   jsonFile,
		Size:       int64(len(content)),
		Mode:       0666,
		Typeflag:   tar.TypeReg,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	})

	if err != nil {
		fmt.Println("here")
		return err
	}

	if _, err := imgwriter.Write(content); err != nil {
		fmt.Println("here")
		return err
	}

	content, err = json.Marshal(repos)
	if err != nil {
		return err
	}

	err = imgwriter.WriteHeader(&tar.Header{
		Name:       "repositories",
		Linkname:   "repositories",
		Uname:      "root",
		Gname:      "root",
		Size:       int64(len(content)),
		Mode:       0666,
		Typeflag:   tar.TypeReg,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	})

	if err != nil {
		fmt.Println("here")
		return err
	}

	if _, err := imgwriter.Write(content); err != nil {
		fmt.Println("here")
		return err
	}

	content, err = json.Marshal(manifest)
	if err != nil {
		return err
	}

	err = imgwriter.WriteHeader(&tar.Header{
		Name:       "manifest.json",
		Linkname:   "manifest.json",
		Uname:      "root",
		Gname:      "root",
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
		Size:       int64(len(content)),
		Mode:       0666,
		Typeflag:   tar.TypeReg,
	})

	if err != nil {
		fmt.Println("here")
		return err
	}

	if _, err := imgwriter.Write(content); err != nil {
		fmt.Println("here")
		return err
	}

	imgwriter.Close()
	w.Close()

	/*
		fi, err := os.Stat(tf.Name())
		if err != nil {
			cancel()
			errChan <- err
			return
		}

		err = imgwriter.WriteHeader(&tar.Header{
			Name:     copyID,
			Mode:     0777,
			Typeflag: tar.TypeDir,
		})

		if err != nil {
			cancel()
			errChan <- err
			return
		}

			err = imgwriter.WriteHeader(&tar.Header{
				Name:     tarFile,
				Size:     fi.Size(),
				Mode:     0666,
				Typeflag: tar.TypeReg,
			})

			if err != nil {
				cancel()
				errChan <- err
				return
			}

			tr, err := os.Open(tf.Name())
			if err != nil {
				cancel()
				errChan <- err
				return
			}

			defer tr.Close()

			x, err := io.Copy(imgwriter, tr)
			fmt.Println(x, err)
			if err != nil {
				cancel()
				errChan <- err
				return
			}
	*/

	if err := <-errChan; err != nil {
		return err
	}

	d.config.Image = copyID

	return nil
}

// Tag an image with the provided string.
func (d *Docker) Tag(tag string) error {
	return d.client.ImageTag(context.Background(), d.config.Image, tag)
}

// Fetch retrieves a docker image, overwrites the container configuration, and returns its id.
func (d *Docker) Fetch(name string) (string, error) {
	inspect, _, err := d.client.ImageInspectWithRaw(context.Background(), name)
	if err != nil {
		reader, err := d.client.ImagePull(context.Background(), name, types.ImagePullOptions{})
		if err != nil {
			return "", err
		}

		if !d.tty {
			fmt.Printf("+++ Pulling %q...", name)
			os.Stdout.Sync()
			_, err := ioutil.ReadAll(reader)
			if err != nil {
				return "", err
			}
			fmt.Println("done.")
		} else {
			if err := printPull(reader); err != nil {
				return "", err
			}
		}

		// this will fallthrough to the assignment below
		inspect, _, err = d.client.ImageInspectWithRaw(context.Background(), name)
		if err != nil {
			return "", err
		}
	}

	d.config.FromDocker(inspect.Config)

	return inspect.ID, nil
}

// RunHook is the run hook for docker agents.
func (d *Docker) RunHook(id string) (string, error) {
	cearesp, err := d.client.ContainerAttach(context.Background(), id, types.ContainerAttachOptions{Stream: true, Stdin: d.stdin, Stdout: true, Stderr: true})
	if err != nil {
		return "", fmt.Errorf("Could not attach to container: %v", err)
	}

	stopChan := make(chan struct{})
	errChan := make(chan error, 1)

	if d.stdin {
		state, err := term.SetRawTerminal(0)
		if err != nil {
			return "", fmt.Errorf("Could not attach terminal to container: %v", err)
		}

		defer term.RestoreTerminal(0, state)

		go doCopy(cearesp.Conn, os.Stdin, errChan, stopChan)
	}

	defer cearesp.Close()

	err = d.client.ContainerStart(context.Background(), id, types.ContainerStartOptions{})
	if err != nil {
		return "", fmt.Errorf("Could not start container: %v", err)
	}

	if !d.stdin {
		color.New(color.FgRed, color.Bold, color.BgWhite).Printf("------ BEGIN OUTPUT ------\n")
	}

	if !d.tty {
		go func() {
			// docker mux's the streams, and requires this stdcopy library to unpack them.
			_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, cearesp.Reader)
			if err != nil && err != io.EOF {
				select {
				case <-stopChan:
					return
				default:
				}

				errChan <- err
			}
		}()
	} else if d.tty {
		go doCopy(os.Stdout, cearesp.Reader, errChan, stopChan)
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		err, ok := <-errChan
		if ok {
			fmt.Printf("+++ Error: %v", err)
			close(stopChan)
			cancel()
		}
	}()

	intSig := make(chan os.Signal)
	signal.Notify(intSig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		_, ok := <-intSig
		if ok {
			fmt.Println("!!! SIGINT or SIGTERM recieved, crashing container...")
			cancel()
		}
	}()

	defer close(intSig)
	defer close(errChan)
	defer close(stopChan)

	stat, err := d.client.ContainerWait(ctx, id)
	if err != nil {
		return "", err
	}

	if !d.stdin {
		color.New(color.FgRed, color.Bold, color.BgWhite).Printf("------- END OUTPUT -------\n")
	}

	if stat != 0 {
		return "", fmt.Errorf("Command exited with status %d for container %q", stat, id)
	}

	return "", nil
}

func printPull(reader io.Reader) error {
	idmap := map[string][]string{}
	idlist := []string{}

	fmt.Println()

	buf := bufio.NewReader(reader)
	for {
		line, err := buf.ReadBytes('\n')
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		var unpacked map[string]interface{}
		if err := json.Unmarshal(line, &unpacked); err != nil {
			return err
		}

		progress, ok := unpacked["progress"].(string)
		if !ok {
			progress = ""
		}

		status := unpacked["status"].(string)
		id, ok := unpacked["id"].(string)
		if !ok {
			fmt.Printf("\x1b[%dA", len(idmap)+1)
			fmt.Printf("\r\x1b[K%s\n", status)
		} else {
			fmt.Printf("\x1b[%dA", len(idmap))
			if _, ok := idmap[id]; !ok {
				idlist = append(idlist, id)
			}

			idmap[id] = []string{status, progress}
		}

		for _, id := range idlist {
			fmt.Printf("\r\x1b[K%s %s %s\n", id, idmap[id][0], idmap[id][1])
		}
	}

	return nil
}

func doCopy(wtr io.Writer, rdr io.Reader, errChan chan error, stopChan chan struct{}) {
	// repeat copy until error is returned. if error is not io.EOF, forward
	// to channel. Return on any error.
	for {
		select {
		case <-stopChan:
			return
		default:
		}

		if _, err := io.Copy(wtr, rdr); err == nil {
			continue
		} else if err != io.EOF {
			select {
			case <-stopChan:
			default:
				errChan <- err
			}
		}

		return
	}
}
