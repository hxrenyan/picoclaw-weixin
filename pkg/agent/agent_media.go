// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/h2non/filetype"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// genericPlaceholderRegex matches generic media placeholders emitted by various
// channels: [image], [image: photo], [image: filename.jpg] — but NOT path tags
// like [image:/path/to/file] (path tags have no space after the colon).
var (
	imagePlaceholderRegex = regexp.MustCompile(`\[image(:\s+[^\]]*)?\]`)
	audioPlaceholderRegex = regexp.MustCompile(`\[audio(:\s+[^\]]*)?\]`)
	videoPlaceholderRegex = regexp.MustCompile(`\[video(:\s+[^\]]*)?\]`)
	filePlaceholderRegex  = regexp.MustCompile(`\[file(:\s+[^\]]*)?\]`)
)

func normalizeCurrentTurnStart(messages []providers.Message, currentTurnStart int) int {
	if currentTurnStart < 0 {
		return 0
	}
	if currentTurnStart > len(messages) {
		return len(messages)
	}
	return currentTurnStart
}

func currentTurnMessages(messages []providers.Message, currentTurnStart int) []providers.Message {
	currentTurnStart = normalizeCurrentTurnStart(messages, currentTurnStart)
	return messages[currentTurnStart:]
}

// resolveMediaRefs resolves media:// refs in messages.
// For user messages: images get path tags only ([image:/path]) so the LLM
// can decide whether to view them via load_image or operate on the file.
// For tool messages: images are base64-encoded and appended as a synthetic
// user message only after the contiguous tool-message block ends, so we don't
// break the tool-results-must-immediately-follow-assistant constraint that
// LLM APIs enforce.
// Only tool messages from the current turn may emit the synthetic user
// follow-up; historical tool results stay as plain path-tagged history.
// Non-image files always get path tags regardless of role.
// Returns a new slice; original messages are not mutated.
func resolveMediaRefs(
	messages []providers.Message,
	store media.MediaStore,
	maxSize int,
	currentTurnStart int,
) []providers.Message {
	return resolveMediaRefsForAgent(messages, store, maxSize, currentTurnStart, nil)
}

// resolveMediaRefsForAgent 校验隔离附件的归属，再复制进所属 Agent 的工作目录。
func resolveMediaRefsForAgent(
	messages []providers.Message,
	store media.MediaStore,
	maxSize int,
	currentTurnStart int,
	agent *AgentInstance,
) []providers.Message {
	if store == nil {
		return messages
	}
	currentTurnStart = normalizeCurrentTurnStart(messages, currentTurnStart)

	result := make([]providers.Message, 0, len(messages))
	var pendingToolImages []string

	for idx, m := range messages {
		// When leaving a tool-message block, flush any accumulated images
		// as a synthetic user message.
		if m.Role != "tool" && len(pendingToolImages) > 0 {
			result = append(result, toolImageFollowUpPromptMessage(pendingToolImages))
			pendingToolImages = nil
		}

		if len(m.Media) == 0 {
			result = append(result, m)
			if idx == len(messages)-1 && len(pendingToolImages) > 0 {
				result = append(result, toolImageFollowUpPromptMessage(pendingToolImages))
				pendingToolImages = nil
			}
			continue
		}

		msg := m
		resolved := make([]string, 0, len(m.Media))
		var pathTags []string

		for _, ref := range m.Media {
			if !strings.HasPrefix(ref, "media://") {
				resolved = append(resolved, ref)
				continue
			}

			localPath, meta, err := store.ResolveWithMeta(ref)
			if err != nil {
				fields := map[string]any{
					"ref":   ref,
					"error": err.Error(),
				}
				if idx < currentTurnStart {
					logger.DebugCF("agent", "Skipped stale historical media ref", fields)
				} else {
					logger.WarnCF("agent", "Failed to resolve media ref", fields)
				}
				continue
			}
			if agent != nil && agent.Isolated {
				if meta.OwnerAgentID != agent.ID {
					logger.WarnCF("agent", "拒绝读取归属不匹配的隔离附件", map[string]any{"ref": ref, "agent_id": agent.ID})
					continue
				}
				localPath, err = copyMediaIntoWorkspace(localPath, ref, agent.Workspace, maxSize)
				if err != nil {
					logger.WarnCF("agent", "复制隔离附件到工作目录失败", map[string]any{
						"ref":      ref,
						"agent_id": agent.ID,
						"error":    err.Error(),
					})
					continue
				}
			}

			info, err := os.Stat(localPath)
			if err != nil {
				logger.WarnCF("agent", "Failed to stat media file", map[string]any{
					"path":  localPath,
					"error": err.Error(),
				})
				continue
			}

			mime := detectMIME(localPath, meta)
			pathTags = append(pathTags, buildPathTag(mime, localPath))

			isolatedUserImage := m.Role == "user" && agent != nil && agent.Isolated
			if (m.Role == "tool" || isolatedUserImage) && idx >= currentTurnStart && strings.HasPrefix(mime, "image/") {
				dataURL := encodeImageToDataURL(localPath, mime, info, maxSize)
				if dataURL != "" {
					if isolatedUserImage {
						// 图片已通过归属检查，直接提供视觉输入，无需开放 load_image。
						resolved = append(resolved, dataURL)
					} else {
						pendingToolImages = append(pendingToolImages, dataURL)
					}
				}
			}
		}

		msg.Media = resolved
		if len(pathTags) > 0 {
			msg.Content = injectPathTags(msg.Content, pathTags)
		}
		result = append(result, msg)

		// If this is the last message and we have pending images, flush them.
		if idx == len(messages)-1 && len(pendingToolImages) > 0 {
			result = append(result, toolImageFollowUpPromptMessage(pendingToolImages))
			pendingToolImages = nil
		}
	}

	return result
}

func copyMediaIntoWorkspace(localPath, ref, workspace string, maxSize int) (string, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer root.Close()
	const dir = ".inbound-media"
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// 同一引用在重建提示词时复用副本，防止压缩和后续回合重复占用磁盘。
	digest := sha256.Sum256([]byte(ref))
	dstName := filepath.Join(dir, fmt.Sprintf("%x%s", digest, filepath.Ext(localPath)))
	if info, err := root.Stat(dstName); err == nil && info.Mode().IsRegular() {
		return filepath.Join(workspace, dstName), nil
	}
	src, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(maxSize) {
		return "", fmt.Errorf("附件不是普通文件或超过大小限制")
	}
	// 使用 os.Root 限制中间目录和目标文件的符号链接，并以重命名原子提交副本。
	tmpName := filepath.Join(dir, uuid.NewString()+".tmp")
	dst, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = dst.Close()
		_ = root.Remove(tmpName)
	}()
	n, err := io.Copy(dst, io.LimitReader(src, int64(maxSize)+1))
	if err != nil {
		return "", err
	}
	if n > int64(maxSize) {
		return "", fmt.Errorf("附件超过大小限制")
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	if err := root.Rename(tmpName, dstName); err != nil {
		return "", err
	}
	return filepath.Join(workspace, dstName), nil
}

// encodeImageToDataURL base64-encodes an image file into a data URL.
// Returns empty string if the file exceeds maxSize or encoding fails.
func encodeImageToDataURL(localPath, mime string, info os.FileInfo, maxSize int) string {
	if info.Size() > int64(maxSize) {
		logger.WarnCF("agent", "Media file too large, skipping", map[string]any{
			"path":     localPath,
			"size":     info.Size(),
			"max_size": maxSize,
		})
		return ""
	}

	f, err := os.Open(localPath)
	if err != nil {
		logger.WarnCF("agent", "Failed to open media file", map[string]any{
			"path":  localPath,
			"error": err.Error(),
		})
		return ""
	}
	defer f.Close()

	prefix := "data:" + mime + ";base64,"
	encodedLen := base64.StdEncoding.EncodedLen(int(info.Size()))
	var buf bytes.Buffer
	buf.Grow(len(prefix) + encodedLen)
	buf.WriteString(prefix)

	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	_, copyErr := io.Copy(encoder, f)
	closeErr := encoder.Close()
	if copyErr != nil {
		logger.WarnCF("agent", "Failed to encode media file", map[string]any{
			"path":  localPath,
			"error": copyErr.Error(),
		})
		return ""
	}
	if closeErr != nil {
		logger.WarnCF("agent", "Failed to close base64 encoder", map[string]any{
			"path":  localPath,
			"error": closeErr.Error(),
		})
		return ""
	}

	return buf.String()
}

func buildArtifactTags(store media.MediaStore, refs []string) []string {
	if store == nil || len(refs) == 0 {
		return nil
	}

	tags := make([]string, 0, len(refs))
	for _, ref := range refs {
		localPath, meta, err := store.ResolveWithMeta(ref)
		if err != nil {
			continue
		}
		mime := detectMIME(localPath, meta)
		tags = append(tags, buildPathTag(mime, localPath))
	}

	return tags
}

func buildProviderAttachments(store media.MediaStore, refs []string) []providers.Attachment {
	if store == nil || len(refs) == 0 {
		return nil
	}

	attachments := make([]providers.Attachment, 0, len(refs))
	for _, ref := range refs {
		attachment := providers.Attachment{Ref: ref}
		if _, meta, err := store.ResolveWithMeta(ref); err == nil {
			attachment.Filename = meta.Filename
			attachment.ContentType = meta.ContentType
			attachment.Type = inferMediaType(meta.Filename, meta.ContentType)
		}
		attachments = append(attachments, attachment)
	}

	return attachments
}

// detectMIME determines the MIME type from metadata or magic-bytes detection.
// Returns empty string if detection fails.
func detectMIME(localPath string, meta media.MediaMeta) string {
	if meta.ContentType != "" {
		return meta.ContentType
	}
	kind, err := filetype.MatchFile(localPath)
	if err != nil || kind == filetype.Unknown {
		return ""
	}
	return kind.MIME.Value
}

// buildPathTag creates a structured tag exposing the local file path.
// Tag type is derived from MIME: [image:/path], [audio:/path], [video:/path], or [file:/path].
func buildPathTag(mime, localPath string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "[image:" + localPath + "]"
	case strings.HasPrefix(mime, "audio/"):
		return "[audio:" + localPath + "]"
	case strings.HasPrefix(mime, "video/"):
		return "[video:" + localPath + "]"
	default:
		return "[file:" + localPath + "]"
	}
}

// injectPathTags replaces generic media tags in content with path-bearing versions,
// or appends if no matching generic tag is found. Channels emit a few different
// placeholder formats — [image], [image: photo], [image: filename.jpg] — so we
// match all of them via regex while leaving path tags ([image:/path]) untouched.
//
// When content is structured data (e.g., JSON from Feishu interactive cards or
// post messages), tags are only injected via placeholder replacement — never
// appended — to avoid corrupting the payload.
func injectPathTags(content string, tags []string) string {
	isStructured := looksLikeJSON(content)
	for _, tag := range tags {
		var pattern *regexp.Regexp
		switch {
		case strings.HasPrefix(tag, "[image:"):
			pattern = imagePlaceholderRegex
		case strings.HasPrefix(tag, "[audio:"):
			pattern = audioPlaceholderRegex
		case strings.HasPrefix(tag, "[video:"):
			pattern = videoPlaceholderRegex
		case strings.HasPrefix(tag, "[file:"):
			pattern = filePlaceholderRegex
		}

		if pattern != nil {
			if loc := pattern.FindStringIndex(content); loc != nil {
				content = content[:loc[0]] + tag + content[loc[1]:]
				continue
			}
		}

		if isStructured {
			content = tag + "\n" + content
			continue
		}

		if content == "" {
			content = tag
		} else {
			content += " " + tag
		}
	}
	return content
}

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) > 1 && s[0] == '{'
}
