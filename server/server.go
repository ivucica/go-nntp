// Package nntpserver provides everything you need for your own NNTP server.
package nntpserver

import (
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/textproto"
	"sort"
	"strconv"
	"strings"

	"github.com/dustin/go-nntp"
)

// An NNTPError is a coded NNTP error message.
type NNTPError struct {
	Code int
	Msg  string
}

// ErrNoSuchGroup is returned for a request for a group that can't be found.
var ErrNoSuchGroup = &NNTPError{411, "No such newsgroup"}

// ErrNoSuchGroup is returned for a request that requires a current
// group when none has been selected.
var ErrNoGroupSelected = &NNTPError{412, "No newsgroup selected"}

// ErrInvalidMessageID is returned when a message is requested that can't be found.
var ErrInvalidMessageID = &NNTPError{430, "No article with that message-id"}

// ErrInvalidArticleNumber is returned when an article is requested that can't be found.
var ErrInvalidArticleNumber = &NNTPError{423, "No article with that number"}

// ErrNoCurrentArticle is returned when a command is executed that
// requires a current article when one has not been selected.
var ErrNoCurrentArticle = &NNTPError{420, "Current article number is invalid"}

// ErrUnknownCommand is returned for unknown comands.
var ErrUnknownCommand = &NNTPError{500, "Unknown command"}

// ErrSyntax is returned when a command can't be parsed.
var ErrSyntax = &NNTPError{501, "not supported, or syntax error"}

// ErrPostingNotPermitted is returned as the response to an attempt to
// post an article where posting is not permitted.
var ErrPostingNotPermitted = &NNTPError{440, "Posting not permitted"}

// ErrPostingFailed is returned when an attempt to post an article fails.
var ErrPostingFailed = &NNTPError{441, "posting failed"}

// ErrNotWanted is returned when an attempt to post an article is
// rejected due the server not wanting the article.
var ErrNotWanted = &NNTPError{435, "Article not wanted"}

// ErrAuthRequired is returned to indicate authentication is required
// to proceed.
var ErrAuthRequired = &NNTPError{450, "authorization required"}

// ErrAuthRejected is returned for invalid authentication.
var ErrAuthRejected = &NNTPError{452, "authorization rejected"}

// ErrNotAuthenticated is returned when a command is issued that requires
// authentication, but authentication was not provided.
var ErrNotAuthenticated = &NNTPError{480, "authentication required"}

// Handler is a low-level protocol handler
type Handler func(args []string, s *session, c *textproto.Conn) error

// A NumberedArticle provides local sequence nubers to articles When
// listing articles in a group.
type NumberedArticle struct {
	Num     int64
	Article *nntp.Article
}

// The Backend that provides the things and does the stuff.
type Backend interface {
	ListGroups(max int) ([]*nntp.Group, error)
	GetGroup(name string) (*nntp.Group, error)
	GetArticle(group *nntp.Group, id string) (*nntp.Article, error)
	GetArticles(group *nntp.Group, from, to int64) ([]NumberedArticle, error)
	Authorized() bool
	// Authenticate and optionally swap out the backend for this session.
	// You may return nil to continue using the same backend.
	Authenticate(user, pass string) (Backend, error)
	AllowPost() bool
	Post(article *nntp.Article) error
}

type session struct {
	server  *Server
	backend Backend
	group   *nntp.Group
}

// The Server handle.
type Server struct {
	// Handlers are dispatched by command name.
	Handlers map[string]Handler
	// The backend (your code) that provides data
	Backend Backend
	// The currently selected group.
	group *nntp.Group
}

// NewServer builds a new server handle request to a backend.
func NewServer(backend Backend) *Server {
	rv := Server{
		Handlers: make(map[string]Handler),
		Backend:  backend,
	}
	rv.Handlers[""] = handleDefault
	rv.Handlers["quit"] = handleQuit
	rv.Handlers["group"] = handleGroup
	rv.Handlers["listgroup"] = handleListGroup
	rv.Handlers["list"] = handleList
	rv.Handlers["head"] = handleHead
	rv.Handlers["body"] = handleBody
	rv.Handlers["article"] = handleArticle
	rv.Handlers["post"] = handlePost
	rv.Handlers["ihave"] = handleIHave
	rv.Handlers["capabilities"] = handleCap
	rv.Handlers["mode"] = handleMode
	rv.Handlers["authinfo"] = handleAuthInfo
	rv.Handlers["newgroups"] = handleNewGroups
	rv.Handlers["over"] = handleOver
	rv.Handlers["xover"] = handleOver
	return &rv
}

func (e *NNTPError) Error() string {
	return fmt.Sprintf("%d %s", e.Code, e.Msg)
}

func (s *session) dispatchCommand(cmd string, args []string,
	c *textproto.Conn) (err error) {

	handler, found := s.server.Handlers[strings.ToLower(cmd)]
	if !found {
		handler, found = s.server.Handlers[""]
		if !found {
			panic("No default handler.")
		}
	}
	return handler(args, s, c)
}

// Process an NNTP session.
func (s *Server) Process(nc net.Conn) {
	defer nc.Close()
	c := textproto.NewConn(nc)

	sess := &session{
		server:  s,
		backend: s.Backend,
		group:   nil,
	}

	c.PrintfLine("200 Hello!")
	for {
		l, err := c.ReadLine()
		if err != nil {
			log.Printf("Error reading from client, dropping conn: %v", err)
			return
		}
		cmd := strings.Split(l, " ")
		log.Printf("Got cmd:  %+v", cmd)
		args := []string{}
		if len(cmd) > 1 {
			args = cmd[1:]
		}
		err = sess.dispatchCommand(cmd[0], args, c)
		if err != nil {
			_, isNNTPError := err.(*NNTPError)
			switch {
			case err == io.EOF:
				// Drop this connection silently. They hung up
				return
			case isNNTPError:
				c.PrintfLine(err.Error())
			default:
				log.Printf("Error dispatching command, dropping conn: %v",
					err)
				return
			}
		}
	}
}

func parseRange(spec string) (low, high int64) {
	if spec == "" {
		return 0, math.MaxInt64
	}
	parts := strings.Split(spec, "-")
	if len(parts) == 1 {
		h, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			h = math.MaxInt64
		}
		return 0, h
	}
	l, _ := strconv.ParseInt(parts[0], 10, 64)
	h, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		h = math.MaxInt64
	}
	return l, h
}

/*
   "0" or article number (see below)
   Subject header content
   From header content
   Date header content
   Message-ID header content
   References header content
   :bytes metadata item
   :lines metadata item
*/

func handleOver(args []string, s *session, c *textproto.Conn) error {
	if s.group == nil {
		return ErrNoGroupSelected
	}
	from, to := parseRange(args[0])
	articles, err := s.backend.GetArticles(s.group, from, to)
	if err != nil {
		return err
	}
	c.PrintfLine("224 here it comes")
	dw := c.DotWriter()
	defer dw.Close()
	for _, a := range articles {
		fmt.Fprintf(dw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", a.Num,
			a.Article.Header.Get("Subject"),
			a.Article.Header.Get("From"),
			a.Article.Header.Get("Date"),
			a.Article.Header.Get("Message-Id"),
			a.Article.Header.Get("References"),
			a.Article.Bytes, a.Article.Lines)
	}
	return nil
}

func handleListOverviewFmt(c *textproto.Conn) error {
	err := c.PrintfLine("215 Order of fields in overview database.")
	if err != nil {
		return err
	}
	dw := c.DotWriter()
	defer dw.Close()
	_, err = fmt.Fprintln(dw, `Subject:
From:
Date:
Message-ID:
References:
:bytes
:lines`)
	return err
}

func handleList(args []string, s *session, c *textproto.Conn) error {
	ltype := "active"
	if len(args) > 0 {
		ltype = strings.ToLower(args[0])
	}

	if ltype == "overview.fmt" {
		return handleListOverviewFmt(c)
	}

	groups, err := s.backend.ListGroups(-1)
	if err != nil {
		return err
	}
	c.PrintfLine("215 list of newsgroups follows")
	dw := c.DotWriter()
	defer dw.Close()
	for _, g := range groups {
		switch ltype {
		case "active":
			fmt.Fprintf(dw, "%s %d %d %v\r\n",
				g.Name, g.High, g.Low, g.Posting)
		case "newsgroups":
			fmt.Fprintf(dw, "%s %s\r\n", g.Name, g.Description)
		}
	}

	return nil
}

func handleNewGroups(args []string, s *session, c *textproto.Conn) error {
	c.PrintfLine("231 list of newsgroups follows")
	c.PrintfLine(".")
	return nil
}

func handleDefault(args []string, s *session, c *textproto.Conn) error {
	return ErrUnknownCommand
}

func handleQuit(args []string, s *session, c *textproto.Conn) error {
	c.PrintfLine("205 bye")
	return io.EOF
}

func handleGroup(args []string, s *session, c *textproto.Conn) error {
	if len(args) < 1 {
		return ErrNoSuchGroup
	}

	group, err := s.backend.GetGroup(args[0])
	if err != nil {
		return err
	}

	s.group = group

	c.PrintfLine("211 %d %d %d %s",
		group.Count, group.Low, group.High, group.Name)
	return nil
}

/*
   Syntax
     LISTGROUP group start-end
     LISTGROUP group start-
     LISTGROUP group start
     LISTGROUP group
     LISTGROUP

   Responses
     211 number low high group  Article numbers follow (multi-line)
     411                        No such newsgroup
     412                        No newsgroup selected
*/

func handleListGroup(args []string, s *session, c *textproto.Conn) error {
	// LISTGROUP: required by Neomutt.
	//
	// Essentially a combination of GROUP and OVER:
	// - accept optional group
	// - if group is accepted, accept range
	// - select the Low article as the current one (even if out of range)
	// - instead of all fields in OVER, print out just the article num

	// n.b. technically RFC3977 does not document permission to return
	// ErrSyntax, but it's the closest we can get to if someone passes
	// invalid range (and this is probably what should be done by
	// parseRange if it is not happening already)

	var group *nntp.Group
	if len(args) < 1 {
		if s.group == nil {
			return ErrNoGroupSelected
		}
		// no group passed? try with the default one
		group = s.group
	}
	// if no group is selected by this point, agent has passed in a group
	// name and we will have to fetch it later

	// process range in one of the formats: N, N-, N-M
	idxRange := "1-" // default per rfc3977 section 6.1.2.2
	if len(args) > 1 {
		idxRange = args[1]
	}

	// Parse the range.
	from, to := parseRange(idxRange)
	if from == 0 {
	    // Error indicator. We do not reach the point where we can pass an
	    // empty value to idxRange (and if we do, it's also invalid), so
	    // 'from' can only be 0 if it's invalid input.
	    return ErrSyntax
	}
	// Cover other invalid values.
	if from < 1 || from > to {
	    return ErrSyntax
	}

	// Parsing syntax complete, start actual work.

	if group == nil {
		// no group selected at this point? user passed a group in.
		// we need to fetch it.
		var err error
		group, err = s.backend.GetGroup(args[0])
		if err != nil {
			return err
		}
		// this command also selects the group, like GROUP does (it is meant
		// to be identical to GROUP except group argument is optional, and
		// range argument is permitted)
		s.group = group
	}

	articles, err := s.backend.GetArticles(s.group, from, to)
	if err != nil {
		return err
	}

	c.PrintfLine("211 %d %d %d %s list follows",
		group.Count, group.Low, group.High, group.Name)

	// Same as in OVER, except we only provide article's num.
	dw := c.DotWriter()
	defer dw.Close()
	for _, a := range articles {
		fmt.Fprintf(dw, "%d\n", a.Num)
	}

	// like GROUP, this is meant to select the first article as the current
	// one in the group, even if that is not the current one.
	//
	// We should first add support for "current article". Implementation
	// of HEAD and of getArticle suggest there is no support for that right
	// now. 'session' has no indication it supports it. *nntp.Group does
	// not either -- and probably should not anyway, it's a session
	// attribute.
	//
	// s.currentArticle = group.Low

	return nil
}

func (s *session) getArticle(args []string) (*nntp.Article, error) {
	if s.group == nil {
		return nil, ErrNoGroupSelected
	}

	if len(args) == 0 {
		// Many commands allow the concept of a 'current' article and
		// allow args to be empty. This is not supported, and args[0]
		// was previously always accessed.
		//
		// Here we pretend that no article is selected because it is
		// currently not stored anywhere. There is no support for
		// 'current' article. We at least prevent a panic when accessing
		// element of slice that's not present.
		return nil, ErrNoCurrentArticle
	}

	return s.backend.GetArticle(s.group, args[0])
}

func sendHeaders(dw io.Writer, article *nntp.Article) {
	order := []string{
		"Subject", "From", "Date", "Message-Id", "References",
	}

	hasImportantKeys := make(map[string]bool)
	for _, v := range order {
		hasImportantKeys[v] = false
	}

	keys := make([]string, 0, len(article.Header))
	for k := range article.Header {
		if _, importantKey := hasImportantKeys[k]; importantKey {
			hasImportantKeys[k] = true
		} else {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	type kv struct {
		k string
		v []string
	}

	newOrder := make([]kv, 0, len(article.Header))
	for _, k := range order {
		if hasImportantKeys[k] {
			newOrder = append(newOrder, kv{k, article.Header[k]})
		}
	}
	for _, k := range keys {
		newOrder = append(newOrder, kv{k, article.Header[k]})
	}
	for _, hdr := range newOrder {
		fmt.Fprintf(dw, "%s: %s\r\n", hdr.k, hdr.v[0])
		// TODO: does NNTP have a problem with multiple instances of the same header? was the above v[0] intentional?
		if len(hdr.v) > 1 {
			for _, v := range hdr.v[1:] {
				fmt.Fprintf(dw, "%s: %s\r\n", hdr.k, v)
			}
		}
	}
}

/*
   Syntax
     HEAD message-id
     HEAD number
     HEAD


   First form (message-id specified)
     221 0|n message-id    Headers follow (multi-line)
     430                   No article with that message-id

   Second form (article number specified)
     221 n message-id      Headers follow (multi-line)
     412                   No newsgroup selected
     423                   No article with that number

   Third form (current article number used)
     221 n message-id      Headers follow (multi-line)
     412                   No newsgroup selected
     420                   Current article number is invalid
*/

func handleHead(args []string, s *session, c *textproto.Conn) error {
	article, err := s.getArticle(args)
	if err != nil {
		return err
	}
	c.PrintfLine("221 1 %s", article.MessageID())
	dw := c.DotWriter()
	defer dw.Close()

	sendHeaders(dw, article)
	return nil
}

/*
   Syntax
     BODY message-id
     BODY number
     BODY

   Responses

   First form (message-id specified)
     222 0|n message-id    Body follows (multi-line)
     430                   No article with that message-id

   Second form (article number specified)
     222 n message-id      Body follows (multi-line)
     412                   No newsgroup selected
     423                   No article with that number

   Third form (current article number used)
     222 n message-id      Body follows (multi-line)
     412                   No newsgroup selected
     420                   Current article number is invalid

   Parameters
     number        Requested article number
     n             Returned article number
     message-id    Article message-id
*/

func handleBody(args []string, s *session, c *textproto.Conn) error {
	article, err := s.getArticle(args)
	if err != nil {
		return err
	}
	c.PrintfLine("222 1 %s", article.MessageID())
	dw := c.DotWriter()
	defer dw.Close()
	_, err = io.Copy(dw, article.Body)
	return err
}

/*
   Syntax
     ARTICLE message-id
     ARTICLE number
     ARTICLE

   Responses

   First form (message-id specified)
     220 0|n message-id    Article follows (multi-line)
     430                   No article with that message-id

   Second form (article number specified)
     220 n message-id      Article follows (multi-line)
     412                   No newsgroup selected
     423                   No article with that number

   Third form (current article number used)
     220 n message-id      Article follows (multi-line)
     412                   No newsgroup selected
     420                   Current article number is invalid

   Parameters
     number        Requested article number
     n             Returned article number
     message-id    Article message-id
*/

func handleArticle(args []string, s *session, c *textproto.Conn) error {
	article, err := s.getArticle(args)
	if err != nil {
		return err
	}
	c.PrintfLine("220 1 %s", article.MessageID())
	dw := c.DotWriter()
	defer dw.Close()

	sendHeaders(dw, article)

	fmt.Fprintln(dw, "")

	_, err = io.Copy(dw, article.Body)
	return err
}

/*
   Syntax
     POST

   Responses

   Initial responses
     340    Send article to be posted
     440    Posting not permitted

   Subsequent responses
     240    Article received OK
     441    Posting failed
*/

func handlePost(args []string, s *session, c *textproto.Conn) error {
	if !s.backend.AllowPost() {
		return ErrPostingNotPermitted
	}

	c.PrintfLine("340 Go ahead")
	var err error
	var article nntp.Article
	article.Header, err = c.ReadMIMEHeader()
	if err != nil {
		return ErrPostingFailed
	}
	article.Body = c.DotReader()
	err = s.backend.Post(&article)
	if err != nil {
		return err
	}
	c.PrintfLine("240 article received OK")
	return nil
}

func handleIHave(args []string, s *session, c *textproto.Conn) error {
	if !s.backend.AllowPost() {
		return ErrNotWanted
	}

	// XXX:  See if we have it.
	article, err := s.backend.GetArticle(nil, args[0])
	if article != nil {
		return ErrNotWanted
	}

	c.PrintfLine("335 send it")
	article = &nntp.Article{}
	article.Header, err = c.ReadMIMEHeader()
	if err != nil {
		return ErrPostingFailed
	}
	article.Body = c.DotReader()
	err = s.backend.Post(article)
	if err != nil {
		return err
	}
	c.PrintfLine("235 article received OK")
	return nil
}

func handleCap(args []string, s *session, c *textproto.Conn) error {
	c.PrintfLine("101 Capability list:")
	dw := c.DotWriter()
	defer dw.Close()

	fmt.Fprintf(dw, "VERSION 2\n")
	fmt.Fprintf(dw, "READER\n")
	if s.backend.AllowPost() {
		fmt.Fprintf(dw, "POST\n")
		fmt.Fprintf(dw, "IHAVE\n")
	}
	fmt.Fprintf(dw, "OVER\n")
	fmt.Fprintf(dw, "XOVER\n")
	fmt.Fprintf(dw, "LIST ACTIVE NEWSGROUPS OVERVIEW.FMT\n")
	return nil
}

func handleMode(args []string, s *session, c *textproto.Conn) error {
	if s.backend.AllowPost() {
		c.PrintfLine("200 Posting allowed")
	} else {
		c.PrintfLine("201 Posting prohibited")
	}
	return nil
}

func handleAuthInfo(args []string, s *session, c *textproto.Conn) error {
	if len(args) < 2 {
		return ErrSyntax
	}
	if strings.ToLower(args[0]) != "user" {
		return ErrSyntax
	}

	if s.backend.Authorized() {
		return c.PrintfLine("250 authenticated")
	}

	c.PrintfLine("350 Continue")
	a, err := c.ReadLine()
	parts := strings.SplitN(a, " ", 3)
	if strings.ToLower(parts[0]) != "authinfo" || strings.ToLower(parts[1]) != "pass" {
		return ErrSyntax
	}
	b, err := s.backend.Authenticate(args[1], parts[2])
	if err == nil {
		c.PrintfLine("250 authenticated")
		if b != nil {
			s.backend = b
		}
	}
	return err
}
