// Adapted from github.com/emersion/go-imap/v2/imapserver/imapmemserver (MIT,
// Copyright (c) 2013 The Go-IMAP Authors). Changes: copy/move report storage
// errors instead of assuming success.
package imapd

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type (
	user    = User
	mailbox = MailboxView
)

// UserSession is an authenticated session; embedded by the gateway session.
type UserSession struct {
	*user    // immutable
	*mailbox // may be nil
}

func (sess *UserSession) Close() error {
	if sess != nil && sess.mailbox != nil {
		sess.mailbox.Close()
	}
	return nil
}

func (sess *UserSession) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	if sess.mailbox != nil {
		sess.mailbox.Close()
	}
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	sess.mailbox = mbox.NewView()
	return mbox.selectDataLocked(), nil
}

func (sess *UserSession) Unselect() error {
	sess.mailbox.Close()
	sess.mailbox = nil
	return nil
}

func (sess *UserSession) dest(destName string) (*Mailbox, error) {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Source and destination mailboxes are identical"}
	}
	return dest, nil
}

func (sess *UserSession) Copy(numSet imap.NumSet, destName string) (*imap.CopyData, error) {
	dest, err := sess.dest(destName)
	if err != nil {
		return nil, err
	}
	var sourceUIDs, destUIDs imap.UIDSet
	var firstErr error
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		if firstErr != nil {
			return
		}
		data, err := dest.copyMsg(msg)
		if err != nil {
			firstErr = err
			return
		}
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(data.UID)
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return &imap.CopyData{UIDValidity: dest.uidValidity, SourceUIDs: sourceUIDs, DestUIDs: destUIDs}, nil
}

func (sess *UserSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, destName string) error {
	dest, err := sess.dest(destName)
	if err != nil {
		return err
	}
	sess.mailbox.mutex.Lock()
	defer sess.mailbox.mutex.Unlock()

	var sourceUIDs, destUIDs imap.UIDSet
	var firstErr error
	moved := make(map[*message]struct{})
	sess.mailbox.forEachLocked(numSet, func(seqNum uint32, msg *message) {
		if firstErr != nil {
			return
		}
		data, err := dest.copyMsg(msg)
		if err != nil {
			firstErr = err
			return
		}
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(data.UID)
		moved[msg] = struct{}{}
	})
	seqNums := sess.mailbox.expungeLocked(moved, true)
	if len(moved) > 0 {
		if err := w.WriteCopyData(&imap.CopyData{UIDValidity: dest.uidValidity, SourceUIDs: sourceUIDs, DestUIDs: destUIDs}); err != nil {
			return err
		}
	}
	for _, seqNum := range seqNums {
		if err := w.WriteExpunge(sess.mailbox.tracker.EncodeSeqNum(seqNum)); err != nil {
			return err
		}
	}
	return firstErr
}

func (sess *UserSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if sess.mailbox == nil {
		return nil
	}
	return sess.mailbox.Poll(w, allowExpunge)
}

func (sess *UserSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if sess.mailbox == nil {
		<-stop
		return nil
	}
	return sess.mailbox.Idle(w, stop)
}
