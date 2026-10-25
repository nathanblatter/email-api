// Adapted from github.com/emersion/go-imap/v2/imapserver/imapmemserver (MIT,
// Copyright (c) 2013 The Go-IMAP Authors). Changes: one account, folders
// persisted in imap_folders, special folders protected.
package imapd

import (
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

const mailboxDelim rune = '/'

// User is the single mail account (nathan@).
type User struct {
	p *persist

	mutex     sync.Mutex
	mailboxes map[string]*Mailbox
	inbox     *Mailbox
}

func normName(name string) string {
	name = strings.TrimRight(name, string(mailboxDelim))
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

func noMailbox() error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
}

func (u *User) mailbox(name string) (*Mailbox, error) {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	mbox := u.mailboxes[normName(name)]
	if mbox == nil {
		return nil, noMailbox()
	}
	return mbox, nil
}

func (u *User) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	mbox, err := u.mailbox(name)
	if err != nil {
		return nil, err
	}
	return mbox.StatusData(options), nil
}

func (u *User) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: mailboxDelim})
	}
	var l []imap.ListData
	for name, mbox := range u.mailboxes {
		match := false
		for _, pattern := range patterns {
			if imapserver.MatchList(name, mailboxDelim, ref, pattern) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		if data := mbox.list(options); data != nil {
			l = append(l, *data)
		}
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Mailbox < l[j].Mailbox })
	for _, data := range l {
		if err := w.WriteList(&data); err != nil {
			return err
		}
	}
	return nil
}

func (u *User) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	mbox, err := u.mailbox(mailbox)
	if err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	return mbox.appendLiteral(r, options)
}

func (u *User) Create(name string, options *imap.CreateOptions) error {
	name = normName(name)
	if name == "" || strings.ContainsRune(name, '*') || strings.ContainsRune(name, '%') {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Invalid mailbox name"}
	}
	u.mutex.Lock()
	defer u.mutex.Unlock()
	if u.mailboxes[name] != nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}
	uv, err := u.p.createFolder(name)
	if err != nil {
		return err
	}
	mb := newMailbox(name, uv, u.p, false)
	mb.uidNext = u.inbox.p.folderUIDNext()
	u.mailboxes[name] = mb
	return nil
}

func (u *User) Delete(name string) error {
	name = normName(name)
	if specialFolders[name] {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "This mailbox cannot be deleted"}
	}
	u.mutex.Lock()
	defer u.mutex.Unlock()
	if u.mailboxes[name] == nil {
		return noMailbox()
	}
	if err := u.p.deleteFolder(name); err != nil {
		return err
	}
	delete(u.mailboxes, name)
	return nil
}

func (u *User) Rename(oldName, newName string, options *imap.RenameOptions) error {
	oldName, newName = normName(oldName), normName(newName)
	if specialFolders[oldName] || specialFolders[newName] {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "This mailbox cannot be renamed"}
	}
	u.mutex.Lock()
	defer u.mutex.Unlock()
	mbox := u.mailboxes[oldName]
	if mbox == nil {
		return noMailbox()
	}
	if u.mailboxes[newName] != nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}
	if err := u.p.renameFolder(oldName, newName); err != nil {
		return err
	}
	mbox.rename(newName)
	u.mailboxes[newName] = mbox
	delete(u.mailboxes, oldName)
	return nil
}

func (u *User) Subscribe(name string) error   { return u.setSub(name, true) }
func (u *User) Unsubscribe(name string) error { return u.setSub(name, false) }

func (u *User) setSub(name string, sub bool) error {
	mbox, err := u.mailbox(name)
	if err != nil {
		return err
	}
	if err := u.p.setSubscribed(mbox.name, sub); err != nil {
		return err
	}
	mbox.mutex.Lock()
	mbox.subscribed = sub || mbox.inbox
	mbox.mutex.Unlock()
	return nil
}

func (u *User) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Delim: mailboxDelim}}}, nil
}
