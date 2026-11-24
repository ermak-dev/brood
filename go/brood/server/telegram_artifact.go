package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

const artifactNoticeMarker = "[brood artifact version]"

func (c *telegramChannel) sendArtifactNotice(ctx context.Context, client *telegram.Client, link store.TelegramLink, from store.Agent, question, versionID string) error {
	if c.server.artifactOrigin == "" {
		return errors.New("BROOD_PUBLIC_HOST is not configured; the version was saved but no notice was sent")
	}
	v, err := c.server.store.GetArtifact(ctx, from.SwarmID, versionID)
	if err != nil {
		return errors.New("could not open that artifact version in this swarm; no notice was sent")
	}
	session, err := c.server.store.TelegramChatSession(ctx, link.UserID, link.BotUsername, link.ChatID, link.ChatID)
	if err != nil || session == "" {
		return errors.New("the Telegram link changed; no notice was sent")
	}
	text := fmt.Sprintf("%s: %s · version %d\n%s %s\n\n%s", from.Name, v.Name, v.Version, artifactNoticeMarker, c.server.artifactURL(from.SwarmID, v.ID), strings.TrimSpace(question))
	if telegram.TextLength(text) > telegram.MessageLimit {
		return errors.New("artifact notice is too long; shorten the question. No notice was sent")
	}
	sent, err := client.SendMessage(ctx, link.ChatID, text, nil)
	if err != nil {
		return errors.New("Telegram did not confirm the artifact notice; check before resending. The version remains saved")
	}
	err = c.server.store.RememberTelegramArtifactReply(ctx, session, sent.MessageID, store.TelegramArtifactReply{SwarmID: from.SwarmID, ActorUID: from.UID, VersionID: v.ID})
	if err != nil {
		return errors.New("the notice was sent but its reply could not be linked; send a new notice for the same saved version")
	}
	return nil
}
