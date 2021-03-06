package client

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/diamondburned/arikawa/discord"
	"github.com/diamondburned/arikawa/gateway"
	"github.com/tadeokondrak/ircdiscord/internal/ilayer"
	"github.com/tadeokondrak/ircdiscord/internal/render"
	"github.com/tadeokondrak/ircdiscord/internal/session"
)

var pingRegex = regexp.MustCompile(`@[^ ]*`)

func (c *Client) replaceIRCMentions(s string) string {
	return pingRegex.ReplaceAllStringFunc(s, func(match string) string {
		if match == "@" {
			return match
		}
		id := c.session.UserFromName(c.guild, match[1:])
		if !id.Valid() {
			return match
		}
		return fmt.Sprintf("<@%d>", id)
	})
}

func (c *Client) HandleRegister() error {
	if c.session == nil {
		return fmt.Errorf("no session provided")
	}

	me, err := c.session.Me()
	if err != nil {
		return err
	}

	c.ilayer.SetClientPrefix(c.discordUserPrefix(me))

	if err := c.seedState(); err != nil {
		return err
	}

	return nil
}

func (c *Client) HandleNickname(nickname string) (string, error) {
	if !c.ilayer.IsRegistered() {
		return nickname, nil
	}
	if c.isGuild() {
		if err := c.session.ChangeOwnNickname(
			c.guild, nickname); err != nil {
			return "", err
		}
	} else {
		return "",
			fmt.Errorf("cannot change nickname outside of server")
	}
	return nickname, nil
}

func (c *Client) HandleUsername(username string) (string, error) {
	return username, nil
}

func (c *Client) HandleRealname(realname string) (string, error) {
	return realname, nil
}

func (c *Client) HandlePassword(password string) (string, error) {
	if c.session != nil {
		c.session.Unref()
		c.session = nil
	}

	args := strings.SplitN(password, ":", 2)
	session, err := c.sessionFunc(args[0], c.discordDebug)
	if err != nil {
		return "", err
	}

	c.session = session

	if len(args) > 1 {
		snowflake, err := discord.ParseSnowflake(args[1])
		if err != nil {
			return "", err
		}

		guild, err := c.session.Guild(snowflake)
		if err != nil {
			return "", err
		}

		c.session.Gateway.GuildSubscribe(gateway.GuildSubscribeData{
			GuildID: guild.ID,
		})

		c.guild = guild.ID
	}

	return password, nil
}

func (c *Client) HandlePing(nonce string) (string, error) {
	return nonce, nil
}

var ErrAlreadyInChannel = errors.New("already in channel")

func (c *Client) HandleJoin(name string) error {
	if c.ilayer.InChannel(name) {
		return ErrAlreadyInChannel
	}

	var channel *discord.Channel
	var channelName string

	if !c.isGuild() {
		user := c.session.UserFromName(c.guild, name)
		if !user.Valid() {
			return fmt.Errorf("no user named %s found", name)
		}

		var err error
		channel, err = c.session.CreatePrivateChannel(user)
		if err != nil {
			return err
		}

		channelName = name
	} else {
		channelID := c.session.ChannelFromName(c.guild, name)
		if !channelID.Valid() {
			return fmt.Errorf("no channel named %s found", name)
		}

		var err error
		channel, err = c.session.Channel(channelID)
		if err != nil {
			return err
		}

		channelName, err = c.session.ChannelName(c.guild, channel.ID)
		if err != nil {
			return err
		}
	}

	names := []string{}

	cancel := c.session.SubscribeUserList(c.guild,
		func(e *session.UserNameChange) {
			if e.IsInitial {
				names = append(names, e.New)
			} else {
				c.handleUsernameChange(e, channelName)
			}
		})
	c.cancels = append(c.cancels, cancel)

	if err := c.ilayer.Join(channelName, channel.Topic,
		channel.ID.Time(), names); err != nil {
		return err
	}

	backlog, err := c.session.Messages(channel.ID)
	if err != nil {
		return err
	}

	for i := len(backlog) - 1; i >= 0; i-- {
		if err := c.sendDiscordMessage(&backlog[i], false); err != nil {
			return err
		}
	}

	return nil
}

var actionRegex = regexp.MustCompile(`^\x01ACTION (.*)\x01$`)

func (c *Client) HandleMessage(channel, content string) error {
	var channelID discord.Snowflake
	if c.isGuild() {
		channelID = c.session.ChannelFromName(c.guild, channel)
	} else {
		user := c.session.UserFromName(c.guild, channel)
		if !user.Valid() {
			return fmt.Errorf("no user named %s", channel)
		}

		channel, err := c.session.CreatePrivateChannel(user)
		if err != nil {
			return err
		}

		channelID = channel.ID
	}

	if strings.HasPrefix(content, "s/") {
		return c.handleRegexEdit(channel, channelID, content)
	}

	content = actionRegex.ReplaceAllString(content, "*$1*")
	content = c.replaceIRCMentions(content)

	msg, err := c.session.SendMessage(channelID, content, nil)
	if err != nil {
		return err
	}
	c.lastMessageID = msg.ID

	return nil
}

var editRegex = regexp.MustCompile(`^s/((?:\\/|[^/])*)/((?:\\/|[^/])*)(?:/(g?))?$`)

func (c *Client) handleRegexEdit(channelName string,
	channelID discord.Snowflake, content string) error {
	matches := editRegex.FindStringSubmatch(content)
	if matches == nil {
		return fmt.Errorf("invalid replacement")
	}

	regex, err := regexp.Compile(content)
	if err != nil {
		return fmt.Errorf("failed to compile regex: %v", err)
	}

	channel := c.session.ChannelFromName(c.guild,
		strings.TrimPrefix(channelName, "#"))
	if !channel.Valid() {
		return fmt.Errorf("failed to find channel #%s", channelName)
	}

	backlog, err := c.session.Messages(channel)
	if err != nil {
		return err
	}

	me, err := c.session.Me()
	if err != nil {
		return err
	}

	var snowflake discord.Snowflake
	for _, msg := range backlog {
		if msg.Author.ID == me.ID {
			snowflake = msg.ID
			break
		}
	}

	if !snowflake.Valid() {
		return fmt.Errorf("failed to find your message")
	}

	message, err := c.session.Message(channel, snowflake)
	if err != nil {
		return err
	}

	beforeEdit := message.Content

	var result string

	if matches[3] == "g" {
		result = regex.ReplaceAllString(beforeEdit, matches[2])
		if result == beforeEdit {
			return fmt.Errorf("no matches")
		}

	} else {
		match := regex.FindStringSubmatchIndex(beforeEdit)
		if match == nil {
			return fmt.Errorf("no matches")
		}

		dst := []byte{}
		replaced := regex.ExpandString(
			dst, matches[2], beforeEdit, match)

		result = beforeEdit[:match[0]] +
			string(replaced) + beforeEdit[match[1]:]
	}

	_, err = c.session.EditMessage(message.ChannelID, message.ID, string(result), nil, false)

	return err
}

func (c *Client) HandleList() ([]ilayer.ListEntry, error) {
	entries := []ilayer.ListEntry{}
	if c.isGuild() {
		channels, err := c.session.Channels(c.guild)
		if err != nil {
			return nil, err
		}
		for _, channel := range channels {
			if visible, err := c.channelIsVisible(
				&channel); err != nil {
				return nil, err
			} else if !visible {
				continue
			}

			var entry ilayer.ListEntry
			var err error
			entry.Channel, err = c.session.ChannelName(
				c.guild, channel.ID)
			if err != nil {
				return nil, err
			}

			topic := render.Content(c.guild, c.session,
				[]byte(channel.Topic), nil)
			entry.Topic = strings.ReplaceAll(topic, "\n", " ")

			entries = append(entries, entry)
		}
	} else {
		channels, err := c.session.PrivateChannels()
		if err != nil {
			return nil, err
		}

		for _, channel := range channels {
			if channel.Type != discord.DirectMessage {
				continue
			}

			var entry ilayer.ListEntry

			recip := channel.DMRecipients[0]

			entry.Channel, err = c.session.UserName(
				c.guild, recip.ID)
			if err != nil {
				entry.Channel = recip.Username
			}

			entry.Topic = fmt.Sprintf("Direct message with %s#%s",
				recip.Username, recip.Discriminator)

			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (c *Client) HandleWhois(username string) (ilayer.WhoisReply, error) {
	var reply ilayer.WhoisReply

	userID := c.session.UserFromName(c.guild, username)
	if !userID.Valid() {
		return ilayer.WhoisReply{},
			fmt.Errorf("no user named %s found", username)
	}

	user, err := c.session.User(userID)
	if err != nil {
		return ilayer.WhoisReply{}, err
	}

	reply.Prefix = c.discordUserPrefix(user)
	reply.Realname = user.Username

	return reply, nil
}
