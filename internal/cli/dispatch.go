package cli

// helpVersionFlags are the root flags cobra owns: help and version must resolve
// to ccp's own output, never forward through to claude.
var helpVersionFlags = map[string]bool{
	"-h": true, "--help": true,
	"-v": true, "--version": true,
}

// InjectRun rewrites a bare ccp invocation that leads with a claude flag into an
// explicit `ccp run`, so `ccp --resume` behaves like `ccp run --resume`. It fires
// only when args[0] is a flag other than ccp's own help/version flags; bare
// `ccp`, subcommands, positional prompts/typos (preserving cobra's suggestions),
// and `ccp --help`/`--version` all pass through unchanged.
func InjectRun(args []string) []string {
	if len(args) == 0 {
		return args
	}
	first := args[0]
	if first == "" || first[0] != '-' || helpVersionFlags[first] {
		return args
	}
	return append([]string{"run"}, args...)
}
