// Package parse turns whatever the user pastes — an ssh command, user@host,
// host:port, an ssh:// URL or a bare IP — into user, host and port.
package parse

import (
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

type Result struct {
	User         string   `json:"user"`
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	Name         string   `json:"name"`
	IdentityFile string   `json:"identityFile,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

var (
	ErrEmpty = errors.New("nothing to read yet")

	hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9_-]*[A-Za-z0-9])?)*\.?$`)
	userRe     = regexp.MustCompile(`^[^\s:/@]+$`)
)

// ssh options that take a value (ssh(1) synopsis).
const sshArgOpts = "BbcDEeFIiJLlmOopQRSWw"

func Parse(text string) (*Result, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "$"), "#"))
	if text == "" {
		return nil, ErrEmpty
	}
	fields := strings.Fields(text)
	res := &Result{}
	dest := ""

	switch fields[0] {
	case "ssh", "sftp":
		portFlag := byte('p')
		argOpts := sshArgOpts
		if fields[0] == "sftp" {
			portFlag = 'P'
			argOpts = "BbcDFiJlOoPRSs"
		}
		args := fields[1:]
	args:
		for i := 0; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "-") || len(a) < 2 {
				if dest != "" {
					break // first word after the destination is the remote command
				}
				dest = a
				continue
			}
			for j := 1; j < len(a); j++ {
				c := a[j]
				if !strings.ContainsRune(argOpts, rune(c)) {
					continue // boolean flag, possibly clustered (-vA)
				}
				val := a[j+1:]
				if val == "" {
					if i+1 >= len(args) {
						break args
					}
					i++
					val = args[i]
				}
				switch c {
				case portFlag:
					if err := setPort(res, val); err != nil {
						return nil, err
					}
				case 'l':
					res.User = val
				case 'i':
					res.IdentityFile = val
				case 'J':
					res.Warnings = append(res.Warnings, "Jump hosts (-J) aren't supported yet; Cotta will connect directly.")
				case 'o':
					if err := applyOption(res, val); err != nil {
						return nil, err
					}
				}
				break
			}
		}
		if dest == "" {
			return nil, errors.New("that ssh command has no host in it")
		}
	default:
		if len(fields) > 1 {
			return nil, errors.New("couldn't read that — try user@host, host:port or an ssh command")
		}
		dest = fields[0]
	}

	if err := parseDest(res, dest); err != nil {
		return nil, err
	}
	if res.Port == 0 {
		res.Port = 22
	}
	if res.User != "" && !userRe.MatchString(res.User) {
		return nil, errors.New("that user name doesn't look right")
	}
	res.Name = suggestName(res.Host)
	return res, nil
}

func setPort(res *Result, s string) error {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return errors.New("port must be a number between 1 and 65535")
	}
	res.Port = p
	return nil
}

func applyOption(res *Result, kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "port":
		return setPort(res, v)
	case "user":
		res.User = v
	case "identityfile":
		res.IdentityFile = v
	case "proxyjump", "proxycommand":
		res.Warnings = append(res.Warnings, "Jump hosts aren't supported yet; Cotta will connect directly.")
	}
	return nil
}

// parseDest reads [scheme://][user@]host[:port]. Values already set by
// command-line flags win over the ones embedded here, as ssh does.
func parseDest(res *Result, dest string) error {
	for _, scheme := range []string{"ssh://", "sftp://"} {
		if strings.HasPrefix(strings.ToLower(dest), scheme) {
			dest = dest[len(scheme):]
			if i := strings.IndexByte(dest, '/'); i >= 0 {
				dest = dest[:i]
			}
			break
		}
	}
	if i := strings.LastIndexByte(dest, '@'); i >= 0 {
		if res.User == "" {
			res.User = dest[:i]
		}
		dest = dest[i+1:]
	}
	host, port := dest, ""
	switch {
	case strings.HasPrefix(dest, "["):
		end := strings.IndexByte(dest, ']')
		if end < 0 {
			return errors.New("that IPv6 address is missing its closing bracket")
		}
		host = dest[1:end]
		if rest := dest[end+1:]; strings.HasPrefix(rest, ":") {
			port = rest[1:]
		} else if rest != "" {
			return errors.New("couldn't read what follows the IPv6 address")
		}
	case strings.Count(dest, ":") == 1:
		host, port, _ = strings.Cut(dest, ":")
	}
	if port != "" && res.Port == 0 {
		if err := setPort(res, port); err != nil {
			return err
		}
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return errors.New("there's no host in that")
	}
	if _, err := netip.ParseAddr(host); err != nil {
		if len(host) > 253 || !hostnameRe.MatchString(host) {
			return errors.New("that host name doesn't look right")
		}
	}
	res.Host = host
	return nil
}

func suggestName(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return host
	}
	first, _, _ := strings.Cut(host, ".")
	return first
}
