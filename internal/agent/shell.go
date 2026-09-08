package agent

// Shell is a container with no agent: just a login shell.
//
// It is not a placeholder. It is how a human works inside a container
// alongside the agents, and it is what the context and projection tests use
// as a stand-in for an agent, since a shell reads a file exactly as reliably
// as a language model does and far more cheaply.
type Shell struct{}

func (s *Shell) Name() string       { return "shell" }
func (s *Shell) ImageLayer() string { return "" }
func (s *Shell) AuthEnv() []string  { return nil }

func (s *Shell) Capabilities() Caps {
	return Caps{MCP: false, Headless: false, Resume: false, Usage: false, Interactive: true}
}

// Prepare exports the context path so a human (or a script) can find it.
func (s *Shell) Prepare(p Projection) error {
	return writeFileIn(p.Home, ".aurium_env",
		"export AURIUM_CONTEXT_FILE="+p.ContextPath+"\n"+
			"export AURIUM_URL="+p.AuriumURL+"\n")
}

func (s *Shell) LaunchCommand(o StartOpts) []string {
	return interactiveWrap("true")
}

func (s *Shell) HeadlessCommand(prompt string, o ExecOpts) []string { return nil }
