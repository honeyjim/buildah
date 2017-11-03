// +build linux

package buildah

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"

	"github.com/containers/storage/pkg/reexec"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	openChrootedCommand = Package + "-open"
)

func init() {
	reexec.Register(openChrootedCommand, openChrootedFileMain)
}

func openChrootedFileMain() {
	status := 0
	flag.Parse()
	if len(flag.Args()) < 1 {
		os.Exit(1)
	}
	// Our first parameter is the directory to chroot into.
	if err := unix.Chdir(flag.Arg(0)); err != nil {
		fmt.Fprintf(os.Stderr, "chdir(): %v", err)
		os.Exit(1)
	}
	if err := unix.Chroot(flag.Arg(0)); err != nil {
		fmt.Fprintf(os.Stderr, "chroot(): %v", err)
		os.Exit(1)
	}
	// Anything else is a file we want to dump out.
	for _, filename := range flag.Args()[1:] {
		f, err := os.Open(filename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open(%q): %v", filename, err)
			status = 1
			continue
		}
		_, err = io.Copy(os.Stdout, f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read(%q): %v", filename, err)
		}
		f.Close()
	}
	os.Exit(status)
}

func openChrootedFile(rootdir, filename string) (*exec.Cmd, io.ReadCloser, error) {
	// The child process expects a chroot and one or more filenames that
	// will be consulted relative to the chroot directory and concatenated
	// to its stdout.  Start it up.
	cmd := reexec.Command(openChrootedCommand, rootdir, filename)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	err = cmd.Start()
	if err != nil {
		return nil, nil, err
	}
	// Hand back the child's stdout for reading, and the child to reap.
	return cmd, stdout, nil
}

var (
	lookupUser, lookupGroup sync.Mutex
)

type lookupPasswdEntry struct {
	name string
	uid  uint64
	gid  uint64
}
type lookupGroupEntry struct {
	name string
	gid  uint64
}

func readWholeLine(rc *bufio.Reader) ([]byte, error) {
	line, isPrefix, err := rc.ReadLine()
	if err != nil {
		return nil, err
	}
	for isPrefix {
		// We didn't get a whole line.  Keep reading chunks until we find an end of line, and discard them.
		for isPrefix {
			logrus.Debugf("discarding partial line %q", string(line))
			_, isPrefix, err = rc.ReadLine()
			if err != nil {
				return nil, err
			}
		}
		// That last read was the end of a line, so now we try to read the (beginning of?) the next line.
		line, isPrefix, err = rc.ReadLine()
		if err != nil {
			return nil, err
		}
	}
	return line, nil
}

func parseNextPasswd(rc *bufio.Reader) *lookupPasswdEntry {
	line, err := readWholeLine(rc)
	if err != nil {
		return nil
	}
	fields := strings.Split(string(line), ":")
	if len(fields) < 7 {
		return nil
	}
	uid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return nil
	}
	gid, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil {
		return nil
	}
	return &lookupPasswdEntry{
		name: fields[0],
		uid:  uid,
		gid:  gid,
	}
}

func parseNextGroup(rc *bufio.Reader) *lookupGroupEntry {
	line, err := readWholeLine(rc)
	if err != nil {
		return nil
	}
	fields := strings.Split(string(line), ":")
	if len(fields) < 4 {
		return nil
	}
	gid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return nil
	}
	return &lookupGroupEntry{
		name: fields[0],
		gid:  gid,
	}
}

func lookupUserInContainer(rootdir, username string) (uid uint64, gid uint64, err error) {
	cmd, f, err := openChrootedFile(rootdir, "/etc/passwd")
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		_ = cmd.Wait()
	}()
	rc := bufio.NewReader(f)
	defer f.Close()

	lookupUser.Lock()
	defer lookupUser.Unlock()

	pwd := parseNextPasswd(rc)
	for pwd != nil {
		if pwd.name != username {
			pwd = parseNextPasswd(rc)
			continue
		}
		return pwd.uid, pwd.gid, nil
	}

	return 0, 0, user.UnknownUserError(fmt.Sprintf("error looking up user %q", username))
}

func lookupGroupForUIDInContainer(rootdir string, userid uint64) (username string, gid uint64, err error) {
	cmd, f, err := openChrootedFile(rootdir, "/etc/passwd")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = cmd.Wait()
	}()
	rc := bufio.NewReader(f)
	defer f.Close()

	lookupUser.Lock()
	defer lookupUser.Unlock()

	pwd := parseNextPasswd(rc)
	for pwd != nil {
		if pwd.uid != userid {
			pwd = parseNextPasswd(rc)
			continue
		}
		return pwd.name, pwd.gid, nil
	}

	return "", 0, user.UnknownUserError(fmt.Sprintf("error looking up user with UID %d", userid))
}

func lookupGroupInContainer(rootdir, groupname string) (gid uint64, err error) {
	cmd, f, err := openChrootedFile(rootdir, "/etc/group")
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = cmd.Wait()
	}()
	rc := bufio.NewReader(f)
	defer f.Close()

	lookupGroup.Lock()
	defer lookupGroup.Unlock()

	grp := parseNextGroup(rc)
	for grp != nil {
		if grp.name != groupname {
			grp = parseNextGroup(rc)
			continue
		}
		return grp.gid, nil
	}

	return 0, user.UnknownGroupError(fmt.Sprintf("error looking up group %q", groupname))
}
