package ruby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/choria-io/go-choria/choria"
	"github.com/choria-io/go-choria/server"
	"github.com/choria-io/go-config"
	"github.com/choria-io/mcorpc-agent-provider/mcorpc"
	"github.com/choria-io/mcorpc-agent-provider/mcorpc/ddl/agent"
)

const (
	// if ruby agents should be enabled by default
	activationDefault = true
)

// ShimRequest is the request being published to the shim runner
type ShimRequest struct {
	Agent      string           `json:"agent"`
	Action     string           `json:"action"`
	RequestID  string           `json:"requestid"`
	SenderID   string           `json:"senderid"`
	CallerID   string           `json:"callerid"`
	Collective string           `json:"collective"`
	TTL        int              `json:"ttl"`
	Time       int64            `json:"msgtime"`
	Body       *ShimRequestBody `json:"body"`
}

// ShimRequestBody is the body passed to the
type ShimRequestBody struct {
	Agent  string          `json:"agent"`
	Action string          `json:"action"`
	Data   json.RawMessage `json:"data"`
	Caller string          `json:"caller"`
}

// NewRubyAgent creates a shim agent that calls to a old mcollective agent implemented in ruby
func NewRubyAgent(ddl *agent.DDL, mgr server.AgentManager) (*mcorpc.Agent, error) {
	agent := mcorpc.New(ddl.Metadata.Name, ddl.Metadata, mgr.Choria(), mgr.Logger())
	agent.SetActivationChecker(activationCheck(ddl, mgr))

	agent.Log.Debugf("Registering proxy actions for Ruby agent %s: %s", ddl.Metadata.Name, strings.Join(ddl.ActionNames(), ", "))

	for _, action := range ddl.ActionNames() {
		actint, err := ddl.ActionInterface(action)
		if err != nil {
			return nil, err
		}

		agent.MustRegisterAction(actint.Name, rubyAction)
	}

	return agent, nil
}

// checks if the plugin.agent.activate_agent is trueish
func configActivationCheck(agent string, cfg *config.Config, dflt bool) bool {
	opts := "plugin." + agent + ".activate_agent"
	should := dflt

	if cfg.HasOption(opts) {
		val := cfg.Option(opts, "unknown")
		if val != "unknown" {
			should, _ = strToBool(val)
		}
	}

	return should
}

func activationCheck(ddl *agent.DDL, mgr server.AgentManager) mcorpc.ActivationChecker {
	cfg := mgr.Choria().Configuration()
	should := configActivationCheck(ddl.Metadata.Name, cfg, activationDefault)

	return func() bool { return should }
}

func rubyAction(ctx context.Context, req *mcorpc.Request, reply *mcorpc.Reply, agent *mcorpc.Agent, conn choria.ConnectorInfo) {
	action := fmt.Sprintf("%s#%s", req.Agent, req.Action)
	shim := agent.Config.Choria.RubyAgentShim
	shimcfg := agent.Config.Choria.RubyAgentConfig

	agent.Log.Debugf("Attempting to call Ruby agent %s with a timeout %d", action, agent.Metadata().Timeout)

	if shim == "" {
		abortAction(fmt.Sprintf("Cannot call Ruby action %s: Ruby compatability shim was not configured", action), agent, reply)
		return
	}

	if shimcfg == "" {
		abortAction(fmt.Sprintf("Cannot call Ruby action %s: Ruby compatability shim configuration file not configured", action), agent, reply)
		return
	}

	if _, err := os.Stat(shim); os.IsNotExist(err) {
		abortAction(fmt.Sprintf("Cannot call Ruby action %s: Ruby compatability shim was not found in %s", action, shim), agent, reply)
		return
	}

	if _, err := os.Stat(shimcfg); os.IsNotExist(err) {
		abortAction(fmt.Sprintf("Cannot call Ruby action %s: Ruby compatability shim configuration file was not found in %s", action, shimcfg), agent, reply)
		return
	}

	tctx, cancel := context.WithTimeout(ctx, time.Duration(time.Duration(agent.Metadata().Timeout)*time.Second))
	defer cancel()

	execution := exec.CommandContext(tctx, agent.Config.Choria.RubyAgentShim, "--config", shimcfg)

	stdin, err := execution.StdinPipe()
	if err != nil {
		abortAction(fmt.Sprintf("Cannot create stdin while calling Ruby action %s: %s", action, err), agent, reply)
		return
	}

	shimr, err := newShimRequest(req)
	if err != nil {
		abortAction(fmt.Sprintf("Cannot prepare request data for Ruby action %s: %s", action, err), agent, reply)
		return
	}

	go func() {
		defer stdin.Close()
		io.WriteString(stdin, string(shimr))
	}()

	stdout, err := execution.StdoutPipe()
	if err != nil {
		abortAction(fmt.Sprintf("Cannot open STDOUT from the Shim for Ruby action %s: %s", action, err), agent, reply)
		return
	}

	err = execution.Start()
	if err != nil {
		abortAction(fmt.Sprintf("Cannot start the Shim for Ruby action %s: %s", action, err), agent, reply)
		return
	}

	if err := json.NewDecoder(stdout).Decode(reply); err != nil {
		abortAction(fmt.Sprintf("Cannot decode output from Shim for Ruby action %s: %s", action, err), agent, reply)
		return
	}

	go func() {
		execution.Wait()
	}()
}

func newShimRequest(req *mcorpc.Request) ([]byte, error) {
	sr := ShimRequest{
		Action: req.Action,
		Agent:  req.Agent,
		Body: &ShimRequestBody{
			Action: req.Action,
			Agent:  req.Agent,
			Caller: req.CallerID,
			Data:   req.Data,
		},
		CallerID:   req.CallerID,
		Collective: req.Collective,
		RequestID:  req.RequestID,
		SenderID:   req.SenderID,
		Time:       req.Time.Unix(),
		TTL:        req.TTL,
	}

	return json.Marshal(sr)
}

func abortAction(reason string, agent *mcorpc.Agent, reply *mcorpc.Reply) {
	agent.Log.Error(reason)
	reply.Statuscode = mcorpc.Aborted
	reply.Statusmsg = reason
}

func strToBool(s string) (bool, error) {
	clean := strings.TrimSpace(s)

	if regexp.MustCompile(`(?i)^(1|yes|true|y|t)$`).MatchString(clean) {
		return true, nil
	}

	if regexp.MustCompile(`(?i)^(0|no|false|n|f)$`).MatchString(clean) {
		return false, nil
	}

	return false, errors.New("cannot convert string value '" + clean + "' into a boolean.")
}
