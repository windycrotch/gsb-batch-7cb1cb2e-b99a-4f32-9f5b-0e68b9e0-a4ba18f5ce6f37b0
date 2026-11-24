package service

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

const (
	defaultVideoCodec      = "avc"
	defaultAudioCodec      = "aac"
	defaultFramerate       = 24
	defaultSegmentDuration = 3
)

type CommandConfig struct {
	DefaultVideoProfiles []VideoProfile
	DefaultAudioProfiles []AudioProfile
}

func NewDefaultCommandConfig() *CommandConfig {
	return &CommandConfig{
		//nolint:gomnd // values are self explanatory
		DefaultVideoProfiles: []VideoProfile{
			{
				Codec:          defaultVideoCodec,
				Bitrate:        500,
				MaxKeyInterval: defaultSegmentDuration * defaultFramerate,
				Framerate:      defaultFramerate,
				Width:          640,
				Height:         360,
			},
			{
				Codec:          defaultVideoCodec,
				Bitrate:        800,
				MaxKeyInterval: defaultSegmentDuration * defaultFramerate,
				Framerate:      defaultFramerate,
				Width:          842,
				Height:         480,
			},
			{
				Codec:          defaultVideoCodec,
				Bitrate:        1200,
				MaxKeyInterval: defaultSegmentDuration * defaultFramerate,
				Framerate:      defaultFramerate,
				Width:          1280,
				Height:         720,
			},
			{
				Codec:          defaultVideoCodec,
				Bitrate:        2500,
				MaxKeyInterval: defaultSegmentDuration * defaultFramerate,
				Framerate:      defaultFramerate,
				Width:          1920,
				Height:         1080,
			},
		},
		//nolint:gomnd // values are self explanatory
		DefaultAudioProfiles: []AudioProfile{
			{
				Codec:   defaultAudioCodec,
				Bitrate: 64,
			},
		},
	}
}

type GPACCommandBuilder struct {
	config *CommandConfig
}

func NewGPACCommandBuilder(config *CommandConfig) *GPACCommandBuilder {
	return &GPACCommandBuilder{config: config}
}

func (b *GPACCommandBuilder) Build(id, address, output string, input Input) *GPACCommand {
	if len(input.VideoProfiles) == 0 {
		input.VideoProfiles = b.config.DefaultVideoProfiles
	}

	if len(input.AudioProfiles) == 0 {
		input.AudioProfiles = b.config.DefaultAudioProfiles
	}

	cmd := &GPACCommand{
		ID:      id,
		Address: address,
		Output:  output,
		Input:   input,
	}
	cmd.Runner = cmd.defaultRunner

	return cmd
}

type GPACCommand struct {
	ID      string
	Address string
	Output  string
	Input   Input

	// Bin optionally overrides the gpac executable name/path. It exists so
	// acceptance tests can point at a local fake gpac script/binary. When
	// empty, "gpac" is resolved from PATH.
	Bin string
	// Ctx binds the subprocess lifetime. Canceling it kills gpac, which is
	// used when the underlying input is deleted while packaging is running.
	Ctx context.Context

	// Runner is the legacy execution seam. When non-nil, Execute invokes it
	// with the resolved program name and built arguments; this keeps existing
	// argument-asserting tests working without a real subprocess.
	Runner func(program string, args []string) error
}

// program returns the configured gpac binary or the default "gpac".
func (g *GPACCommand) program() string {
	if g.Bin != "" {
		return g.Bin
	}

	return "gpac"
}

func (g *GPACCommand) context() context.Context {
	if g.Ctx != nil {
		return g.Ctx
	}

	return context.Background()
}

// defaultRunner runs gpac as a real subprocess bound to g.Ctx. It is a
// method value so it always observes the current Ctx/Bin fields.
func (g *GPACCommand) defaultRunner(program string, args []string) error {
	return exec.CommandContext(g.context(), program, args...).Run()
}

func (g *GPACCommand) Execute() error {
	args := []string{"-i", fmt.Sprintf("%s/%s", g.Address, g.ID)}

	for i, v := range g.Input.VideoProfiles {
		bitrate := fmt.Sprintf("b=%dk", v.Bitrate)
		profileFlag := "@@"

		if i == 0 {
			profileFlag = "@"
		}

		args = append(args, profileFlag, fmt.Sprintf("c=avc:%s", bitrate))
	}

	for _, a := range g.Input.AudioProfiles {
		args = append(args, "@@", fmt.Sprintf("c=aac:b=%dk", a.Bitrate))
	}

	// connect filters
	args = append(args, "@")
	totalChannels := len(g.Input.VideoProfiles) + len(g.Input.AudioProfiles)

	for i := 1; i < totalChannels; i++ {
		args = append(args, fmt.Sprintf("@%d", i))
	}

	args = append(args, "-o", fmt.Sprintf("%s/%s/index.m3u8:segdur=%d:dmode=dynamic:profile=live:muxtype=ts", g.Output, g.ID, defaultSegmentDuration))

	program := g.program()
	log.Printf("Executing gpac command: %s %s", program, strings.Join(args, " "))

	if g.Runner != nil {
		return g.Runner(program, args)
	}

	return g.defaultRunner(program, args)
}

func NewGPACCommand(id, address, output string, input Input) *GPACCommand {
	command := &GPACCommand{
		ID:      id,
		Address: address,
		Output:  output,
		Input:   input,
	}
	command.Runner = command.defaultRunner

	return command
}
