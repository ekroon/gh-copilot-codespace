package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func runFilesystemOperation(ctx context.Context, op string, in io.Reader, out io.Writer) error {
	encode := func(result any) error {
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return fmt.Errorf("%s: encode response: %w", op, err)
		}
		return nil
	}

	switch op {
	case "view":
		var req ssh.ViewRequest
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		result, err := daemonView(ctx, req)
		if err != nil {
			return err
		}
		return encode(result)
	case "read":
		var req ssh.RootedReadRequest
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		data, err := daemonReadFile(ctx, req.Path, req.Root)
		if err != nil {
			return err
		}
		return encode(daemonproto.ReadFileResult{Data: data})
	case "edit":
		var req daemonproto.EditFileParams
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		return daemonEditFile(ctx, req.Path, req.OldStr, req.NewStr)
	case "create":
		var req daemonproto.CreateFileParams
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		return daemonCreateFile(ctx, req.Path, req.Content, req.AllowOverwrite)
	case "write":
		var req ssh.RootedWriteRequest
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		return daemonWriteFile(ctx, req.Path, req.Data, req.Overwrite, req.Root)
	case "grep":
		var req ssh.GrepRequest
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		result, err := daemonGrepFiles(ctx, req)
		if err != nil {
			return err
		}
		return encode(result)
	case "glob":
		var req ssh.GlobRequest
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		result, err := daemonGlobFiles(ctx, req)
		if err != nil {
			return err
		}
		return encode(result)
	case "apply_patch":
		var req ssh.ApplyPatchRequest
		if err := decodeFilesystemRequest(op, in, &req); err != nil {
			return err
		}
		result, err := daemonApplyPatch(ctx, req)
		if err != nil {
			return err
		}
		return encode(result)
	default:
		return fmt.Errorf("unknown filesystem operation %q", op)
	}
}

func decodeFilesystemRequest(op string, in io.Reader, out any) error {
	decoder := json.NewDecoder(in)
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%s: decode request: %w", op, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s: decode request: trailing JSON value", op)
		}
		return fmt.Errorf("%s: decode request: %w", op, err)
	}
	return nil
}
