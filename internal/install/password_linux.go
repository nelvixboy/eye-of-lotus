//go:build linux

package install

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// stdin is one reader for the whole process. Creating a fresh buffered
// reader for each question would swallow the second answer: the first reader
// reads ahead, and the bytes it has already taken never come back.
var stdin = bufio.NewReader(os.Stdin)

// promptForPassword asks for the passphrase twice and returns it.
//
// Echoing is switched off with a single ioctl rather than by taking a
// dependency on a terminal package. This program has one dependency on
// purpose, and the code to do this is a dozen lines on the only platform it
// runs on.
func promptForPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		// Not a terminal. Reading the line anyway is what makes
		// `echo secret | eye-of-lotus -install` and EOL_PASSWORD work.
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("there is no terminal to ask for a password on; set EOL_PASSWORD instead")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	first, err := readHidden(fd, "    Password for the panel: ")
	if err != nil {
		return "", err
	}
	second, err := readHidden(fd, "    Repeat it: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the passwords do not match")
	}
	return first, nil
}

func isTerminal(fd int) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}

// readHidden prints a prompt and reads a line with echoing turned off,
// restoring the terminal whatever happens.
func readHidden(fd int, prompt string) (string, error) {
	var before syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&before)), 0, 0, 0); errno != 0 {
		return "", errors.New("cannot read the terminal settings")
	}
	during := before
	during.Lflag &^= syscall.ECHO
	during.Lflag |= syscall.ICANON | syscall.ISIG
	during.Iflag |= syscall.ICRNL
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&during)), 0, 0, 0); errno != 0 {
		return "", errors.New("cannot turn off terminal echoing")
	}
	defer syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&before)), 0, 0, 0)

	fmt.Fprint(os.Stderr, prompt)
	line, err := stdin.ReadString('\n')
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
