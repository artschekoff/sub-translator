package main

import (
	"fmt"
	"io"

	"github.com/artschekoff/sub-translator/internal/config"
)

const configUsage = `Usage:
  sub-translator config list              show every setting and its value
  sub-translator config get <key>         print one setting
  sub-translator config set <key> <value> change one setting
  sub-translator config path              print the config file location`

func runConfig(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("config: missing subcommand\n\n%s", configUsage)
	}

	switch args[0] {
	case "path":
		p, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, p)
		return nil

	case "list":
		c, err := config.Load()
		if err != nil {
			return err
		}
		for _, key := range config.Keys() {
			value, err := c.Get(key)
			if err != nil {
				return err
			}
			if value == "" {
				value = "(unset)"
			}
			fmt.Fprintf(out, "  %-22s %s\n", key, value)
		}
		return nil

	case "get":
		if len(args) != 2 {
			return fmt.Errorf("config get: want exactly one key\n\n%s", configUsage)
		}
		c, err := config.Load()
		if err != nil {
			return err
		}
		value, err := c.Get(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(out, value)
		return nil

	case "set":
		if len(args) != 3 {
			return fmt.Errorf("config set: want a key and a value\n\n%s", configUsage)
		}
		c, err := config.Load()
		if err != nil {
			return err
		}
		if err := c.Set(args[1], args[2]); err != nil {
			return err
		}
		if err := c.Save(); err != nil {
			return err
		}
		value, _ := c.Get(args[1])
		fmt.Fprintf(out, "%s = %s\n", args[1], value)
		return nil

	default:
		return fmt.Errorf("config: unknown subcommand %q\n\n%s", args[0], configUsage)
	}
}
