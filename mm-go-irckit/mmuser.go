package irckit

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/mattermost"
	"github.com/davecgh/go-spew/spew"
	"github.com/mattermost/mattermost-server/model"
	"github.com/muesli/reflow/wordwrap"
	"github.com/sorcix/irc"
	"github.com/spf13/viper"
)

type Info struct {
	Srv         Server
	Credentials bridge.Credentials
	br          bridge.Bridger // nolint:structcheck
	inprogress  bool           //nolint:structcheck
}

func NewUserBridge(c net.Conn, srv Server, cfg *viper.Viper) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})

	u.Srv = srv
	u.v = cfg

	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	return u
}

func (u *User) handleEventChan(events chan *bridge.Event) {
	for event := range events {
		spew.Dump("receiving", event)
		switch e := event.Data.(type) {
		case *bridge.ChannelMessageEvent:
			u.handleChannelMessageEvent(e)
		case *bridge.DirectMessageEvent:
			u.handleDirectMessageEvent(e)
		case *bridge.ChannelTopicEvent:
			u.handleChannelTopicEvent(e)
		case *bridge.FileEvent:
			u.handleFileEvent(e)
		case *bridge.ChannelAddEvent:
			u.handleChannelAddEvent(e)
		case *bridge.ChannelRemoveEvent:
			u.handleChannelRemoveEvent(e)
		case *bridge.ChannelCreateEvent:
			u.handleChannelCreateEvent(e)
		case *bridge.ChannelDeleteEvent:
			u.handleChannelDeleteEvent(e)
		}
	}
}

func (u *User) handleChannelTopicEvent(event *bridge.ChannelTopicEvent) {
	tu, _ := u.Srv.HasUser(event.Sender)
	ch := u.Srv.Channel(event.ChannelID)
	ch.Topic(tu, event.Text)
}

func (u *User) handleDirectMessageEvent(event *bridge.DirectMessageEvent) {
	if event.Sender.Me {
		u.MsgSpoofUser(u, event.Receiver, event.Text)
	} else {
		u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver, event.Text)
	}
}

func (u *User) handleChannelAddEvent(event *bridge.ChannelAddEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	for _, added := range event.Added {
		if added.Me {
			u.syncChannel(event.ChannelID, u.br.GetChannelName(event.ChannelID))
			continue
		}

		ghost := u.createUserFromInfo(added)
		ch.Join(ghost)

		ch.SpoofMessage("system", "added "+added.Nick+" to the channel by "+event.Adder.Nick)
	}
}

func (u *User) handleChannelRemoveEvent(event *bridge.ChannelRemoveEvent) {
	spew.Dump(event)

	ch := u.Srv.Channel(event.ChannelID)

	for _, removed := range event.Removed {
		if removed.Me {
			ch.Part(u, "")
			continue
		}

		ghost := u.createUserFromInfo(removed)

		ch.Part(ghost, "")
		if event.Remover != nil {
			ch.SpoofMessage("system", "removed "+removed.Nick+" from the channel by "+event.Remover.Nick)
		} else {
			ch.SpoofMessage("system", "removed "+removed.Nick+" from the channel")
		}
	}
}

func (u *User) getMessageChannel(channelID, channelType string, sender *bridge.UserInfo) Channel {
	ch := u.Srv.Channel(channelID)
	// in an group
	if channelType == "G" {
		myself := u.createUserFromInfo(u.br.GetMe())
		if !ch.HasUser(myself) {
			ch.Join(myself)
			u.syncChannel(channelID, u.br.GetChannelName(channelID))
		}
	}
	ghost := u.createUserFromInfo(sender)
	// join if not in channel
	if !ch.HasUser(ghost) {
		logger.Debugf("User %s is not in channel %s. Joining now", ghost.Nick, ch.String())
		// ch = u.Srv.Channel("&messages")
		ch.Join(ghost)
	}
	// excluded channel
	if stringInSlice(ch.String(), u.v.GetStringSlice(u.br.Protocol()+".joinexclude")) {
		logger.Debugf("channel %s is in JoinExclude, send to &messages", ch.String())
		ch = u.Srv.Channel("&messages")
	}
	// not in included channel
	if len(u.v.GetStringSlice(u.br.Protocol()+".joininclude")) > 0 && !stringInSlice(ch.String(), u.v.GetStringSlice(u.br.Protocol()+".joininclude")) {
		logger.Debugf("channel %s is not in JoinInclude, send to &messages", ch.String())
		ch = u.Srv.Channel("&messages")
	}

	return ch
}

func (u *User) handleChannelMessageEvent(event *bridge.ChannelMessageEvent) {
	/*
			      CHANNEL_OPEN                   = "O"
		        CHANNEL_PRIVATE                = "P"
		        CHANNEL_DIRECT                 = "D"
				CHANNEL_GROUP                  = "G"
	*/
	ch := u.getMessageChannel(event.ChannelID, event.ChannelType, event.Sender)
	if event.Sender.Me {
		event.Sender.Nick = u.Nick
	}

	switch event.MessageType {
	case "notice":
		ch.SpoofNotice(event.Sender.Nick, event.Text)
	default:
		ch.SpoofMessage(event.Sender.Nick, event.Text)
	}
}

func (u *User) handleFileEvent(event *bridge.FileEvent) {
	ch := u.getMessageChannel(event.ChannelID, event.ChannelType, event.Sender)

	switch event.ChannelType {
	case "D":
		for _, fname := range event.Files {
			if event.Sender.Me {
				u.MsgSpoofUser(u, event.Receiver, "download file -"+fname.Name)
			} else {
				u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver, "download file -"+fname.Name)
			}
		}
	default:
		for _, fname := range event.Files {
			if event.Sender.Me {
				ch.SpoofMessage(u.Nick, "download file -"+fname.Name)
			} else {
				ch.SpoofMessage(event.Sender.Nick, "download file -"+fname.Name)
			}
		}
	}
}

func (u *User) handleChannelCreateEvent(event *bridge.ChannelCreateEvent) {
	u.br.UpdateChannels()

	logger.Debugf("ACTION_CHANNEL_CREATED adding myself to %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)

	u.syncChannel(event.ChannelID, u.br.GetChannelName(event.ChannelID))
}

func (u *User) handleChannelDeleteEvent(event *bridge.ChannelDeleteEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	logger.Debugf("ACTION_CHANNEL_DELETED removing myself from %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)

	ch.Part(u, "")
}

func (u *User) loginToMattermost() error {
	eventChan := make(chan *bridge.Event)
	br, _, err := mattermost.New(u.v, u.Credentials, eventChan, u.addUsersToChannels)
	if err != nil {
		return err
	}

	u.br = br

	go u.handleEventChan(eventChan)

	return nil
}

func (u *User) createService(nick string, what string) {
	service := &User{
		UserInfo: &bridge.UserInfo{
			Nick:  nick,
			User:  nick,
			Real:  what,
			Host:  "service",
			Ghost: true,
		},
		channels: map[Channel]struct{}{},
	}

	u.Srv.Add(service)
}

func (u *User) createUserFromInfo(info *bridge.UserInfo) *User {
	if ghost, ok := u.Srv.HasUser(info.Nick); ok {
		return ghost
	}

	ghost := &User{
		UserInfo: info,
		channels: map[Channel]struct{}{},
	}

	u.Srv.Add(ghost)

	return ghost
}

func (u *User) addUsersToChannel(users []*User, channel string, channelID string) {
	logger.Debugf("adding %d to %s", len(users), channel)

	ch := u.Srv.Channel(channelID)

	ch.BatchJoin(users)
}

func (u *User) addUsersToChannels() {
	srv := u.Srv
	throttle := time.NewTicker(time.Millisecond * 50)

	logger.Debug("in addUsersToChannels()")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")

	var batchJoins []*User

	for _, bruser := range u.br.GetUsers() {
		if bruser.Me {
			continue
		}

		batchJoins = append(batchJoins, u.createUserFromInfo(bruser))
		//		ghost := u.createUserFromInfo(bruser)
		//		u.addUserToChannel(ghost, "&users", "&users")
	}

	u.addUsersToChannel(batchJoins, "&users", "&users")

	ch.Join(u)

	// channel that receives messages from channels not joined on irc
	ch = srv.Channel("&messages")
	ch.Join(u)

	channels := make(chan *bridge.ChannelInfo, 5)
	for i := 0; i < 10; i++ {
		go u.addUserToChannelWorker(channels, throttle)
	}

	for _, brchannel := range u.br.GetChannels() {
		logger.Debugf("Adding channel %#v", brchannel)
		channels <- brchannel
	}

	close(channels)
}

func (u *User) createSpoof(mmchannel *bridge.ChannelInfo) func(string, string) {
	if strings.Contains(mmchannel.Name, "__") {
		userID := strings.Split(mmchannel.Name, "__")[0]
		u.createUserFromInfo(u.br.GetUser(userID))
		// wrap MsgSpoofser here
		return func(spoofUsername string, msg string) {
			u.MsgSpoofUser(u, spoofUsername, msg)
		}
	}

	channelName := mmchannel.Name

	if mmchannel.TeamID != u.br.GetMe().TeamID || u.v.GetBool(u.br.Protocol()+".prefixmainteam") {
		channelName = u.br.GetTeamName(mmchannel.TeamID) + "/" + mmchannel.Name
	}

	u.syncChannel(mmchannel.ID, channelName)
	ch := u.Srv.Channel(mmchannel.ID)

	return ch.SpoofMessage
}

func (u *User) addUserToChannelWorker(channels <-chan *bridge.ChannelInfo, throttle *time.Ticker) {
	for brchannel := range channels {
		logger.Debug("addUserToChannelWorker", brchannel)

		<-throttle.C
		// exclude direct messages
		spoof := u.createSpoof(brchannel)

		since := u.br.GetLastViewedAt(brchannel.ID)
		// ignore invalid/deleted/old channels
		if since == 0 {
			continue
		}
		// post everything to the channel you haven't seen yet
		postlist := u.br.GetPostsSince(brchannel.ID, since)
		if postlist == nil {
			// if the channel is not from the primary team id, we can't get posts
			if brchannel.TeamID == u.br.GetMe().TeamID {
				logger.Errorf("something wrong with getPostsSince for channel %s (%s)", brchannel.ID, brchannel.Name)
			}
			continue
		}

		var prevDate string

		mmPostList := postlist.(*model.PostList)
		// traverse the order in reverse
		for i := len(mmPostList.Order) - 1; i >= 0; i-- {
			p := mmPostList.Posts[mmPostList.Order[i]]
			if p.Type == model.POST_JOIN_LEAVE {
				continue
			}

			if p.DeleteAt > p.CreateAt {
				continue
			}

			ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))

			for _, post := range strings.Split(p.Message, "\n") {
				user := u.br.GetUser(p.UserId)
				date := ts.Format("2006-01-02")
				if date != prevDate {
					spoof("matterircd", fmt.Sprintf("Replaying since %s", date))
					prevDate = date
				}

				nick := user.Nick

				spoof(nick, fmt.Sprintf("[%s] %s", ts.Format("15:04"), post))
			}
		}

		if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
			u.br.UpdateLastViewed(brchannel.ID)
		}
	}
}

func (u *User) MsgUser(toUser *User, msg string) {
	u.Encode(&irc.Message{
		Prefix:   toUser.Prefix(),
		Command:  irc.PRIVMSG,
		Params:   []string{u.Nick},
		Trailing: msg,
	})
}

func (u *User) MsgSpoofUser(sender *User, rcvuser string, msg string) {
	msg = wordwrap.String(msg, 440)
	lines := strings.Split(msg, "\n")

	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) == 0 {
			continue
		}

		u.Encode(&irc.Message{
			Prefix: &irc.Prefix{
				Name: sender.Nick,
				User: sender.Nick,
				Host: sender.Host,
			},
			Command:  irc.PRIVMSG,
			Params:   []string{rcvuser},
			Trailing: l + "\n",
		})
	}
}

// sync IRC with mattermost channel state
func (u *User) syncChannel(id string, name string) {
	users, err := u.br.GetChannelUsers(id)
	if err != nil {
		fmt.Println(err)
		return
	}

	srv := u.Srv

	var batchUsers []*User

	for _, ghost := range users {
		if ghost.Me {
			continue
		}

		batchUsers = append(batchUsers, u.createUserFromInfo(ghost))
		//		u.addRealUserToChannel(u.createUserFromInfo(ghost), "#"+name, id)
	}

	u.addUsersToChannel(batchUsers, "#"+name, id)

	for _, ghost := range users {
		if !ghost.Me {
			continue
		}

		ch := srv.Channel(id)
		// only join when we're not yet on the channel
		if ch.HasUser(u) {
			break
		}

		logger.Debugf("syncMMChannel adding myself to %s (id: %s)", name, id)

		if stringInSlice(ch.String(), u.v.GetStringSlice(u.br.Protocol()+".joinexclude")) {
			continue
		}

		ch.Join(u)

		svc, _ := srv.HasUser(u.br.Protocol())

		ch.Topic(svc, u.br.Topic(ch.ID()))
	}
}

func (u *User) isValidMMServer(server string) bool {
	if len(u.v.GetStringSlice("allowedservers")) == 0 {
		return true
	}

	logger.Debugf("allowedservers: %s", u.v.GetStringSlice("allowedservers"))

	for _, srv := range u.v.GetStringSlice("allowedservers") {
		if srv == server {
			return true
		}
	}

	return false
}
