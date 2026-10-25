// Adapted from github.com/emersion/go-imap/v2/imapserver/imapmemserver (MIT,
// Copyright (c) 2013 The Go-IMAP Authors). Changes: every mutation (append,
// flag change, expunge) is written through to Postgres before it becomes
// visible, UIDs come from the database, and the inbox can be updated from
// outside (new mail, the web inbox marking read or deleting).
package imapd

import (
	"bytes"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Mailbox is one IMAP folder held in memory and backed by Postgres.
type Mailbox struct {
	tracker     *imapserver.MailboxTracker
	uidValidity uint32
	p           *persist
	inbox       bool

	mutex      sync.Mutex
	name       string
	subscribed bool
	specialUse []imap.MailboxAttr
	l          []*message
	uidNext    imap.UID
}

func newMailbox(name string, uidValidity uint32, p *persist, inbox bool) *Mailbox {
	return &Mailbox{
		tracker:     imapserver.NewMailboxTracker(0),
		uidValidity: uidValidity,
		p:           p,
		inbox:       inbox,
		name:        name,
		subscribed:  true,
		uidNext:     1,
	}
}

func (mbox *Mailbox) list(options *imap.ListOptions) *imap.ListData {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	if options.SelectSubscribed && !mbox.subscribed {
		return nil
	}
	if options.SelectSpecialUse && len(mbox.specialUse) == 0 {
		return nil
	}
	data := imap.ListData{Mailbox: mbox.name, Delim: mailboxDelim}
	if mbox.subscribed {
		data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
	}
	// Apple Mail maps Sent/Drafts/Trash/Archive by these attributes, so they
	// are always returned (harmless to clients that did not ask).
	data.Attrs = append(data.Attrs, mbox.specialUse...)
	if options.ReturnStatus != nil {
		data.Status = mbox.statusDataLocked(options.ReturnStatus)
	}
	return &data
}

// StatusData returns data for the STATUS command.
func (mbox *Mailbox) StatusData(options *imap.StatusOptions) *imap.StatusData {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	return mbox.statusDataLocked(options)
}

func (mbox *Mailbox) statusDataLocked(options *imap.StatusOptions) *imap.StatusData {
	data := imap.StatusData{Mailbox: mbox.name}
	if options.NumMessages {
		num := uint32(len(mbox.l))
		data.NumMessages = &num
	}
	if options.UIDNext {
		data.UIDNext = mbox.uidNext
	}
	if options.UIDValidity {
		data.UIDValidity = mbox.uidValidity
	}
	if options.NumUnseen {
		num := uint32(len(mbox.l)) - mbox.countByFlagLocked(imap.FlagSeen)
		data.NumUnseen = &num
	}
	if options.NumDeleted {
		num := mbox.countByFlagLocked(imap.FlagDeleted)
		data.NumDeleted = &num
	}
	if options.Size {
		size := mbox.sizeLocked()
		data.Size = &size
	}
	if options.NumRecent {
		num := uint32(0)
		data.NumRecent = &num
	}
	return &data
}

func (mbox *Mailbox) countByFlagLocked(flag imap.Flag) uint32 {
	var n uint32
	for _, msg := range mbox.l {
		if _, ok := msg.flags[canonicalFlag(flag)]; ok {
			n++
		}
	}
	return n
}

func (mbox *Mailbox) sizeLocked() int64 {
	var size int64
	for _, msg := range mbox.l {
		size += int64(len(msg.buf))
	}
	return size
}

func (mbox *Mailbox) appendLiteral(r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return mbox.appendBytes(buf.Bytes(), options)
}

func (mbox *Mailbox) copyMsg(msg *message) (*imap.AppendData, error) {
	return mbox.appendBytes(msg.buf, &imap.AppendOptions{Time: msg.t, Flags: msg.flagList()})
}

// appendBytes stores the message first, then shows it to clients.
func (mbox *Mailbox) appendBytes(buf []byte, options *imap.AppendOptions) (*imap.AppendData, error) {
	msg := &message{flags: make(map[imap.Flag]struct{}), buf: buf, t: options.Time}
	if msg.t.IsZero() {
		msg.t = time.Now()
	}
	for _, flag := range options.Flags {
		msg.flags[canonicalFlag(flag)] = struct{}{}
	}
	uid, id, err := mbox.p.append(mbox, msg)
	if err != nil {
		return nil, err
	}
	msg.uid, msg.id = uid, id

	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	mbox.insertLocked(msg)
	return &imap.AppendData{UIDValidity: mbox.uidValidity, UID: msg.uid}, nil
}

// insertLocked adds a stored message unless it is already present (the
// inbox sync loop and an APPEND can race to add the same row).
func (mbox *Mailbox) insertLocked(msg *message) {
	for _, m := range mbox.l {
		if m.uid == msg.uid {
			return
		}
	}
	mbox.l = append(mbox.l, msg)
	sort.SliceStable(mbox.l, func(i, j int) bool { return mbox.l[i].uid < mbox.l[j].uid })
	if msg.uid >= mbox.uidNext {
		mbox.uidNext = msg.uid + 1
	}
	mbox.tracker.QueueNumMessages(uint32(len(mbox.l)))
}

func (mbox *Mailbox) rename(newName string) {
	mbox.mutex.Lock()
	mbox.name = newName
	mbox.mutex.Unlock()
}

func (mbox *Mailbox) selectDataLocked() *imap.SelectData {
	flags := mbox.flagsLocked()
	permanentFlags := make([]imap.Flag, len(flags))
	copy(permanentFlags, flags)
	permanentFlags = append(permanentFlags, imap.FlagWildcard)
	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    permanentFlags,
		NumMessages:       uint32(len(mbox.l)),
		FirstUnseenSeqNum: mbox.firstUnseenSeqNumLocked(),
		UIDNext:           mbox.uidNext,
		UIDValidity:       mbox.uidValidity,
	}
}

func (mbox *Mailbox) firstUnseenSeqNumLocked() uint32 {
	for i, msg := range mbox.l {
		if _, ok := msg.flags[canonicalFlag(imap.FlagSeen)]; !ok {
			return uint32(i) + 1
		}
	}
	return 0
}

func (mbox *Mailbox) flagsLocked() []imap.Flag {
	m := map[imap.Flag]struct{}{
		canonicalFlag(imap.FlagSeen): {}, canonicalFlag(imap.FlagAnswered): {}, canonicalFlag(imap.FlagFlagged): {},
		canonicalFlag(imap.FlagDeleted): {}, canonicalFlag(imap.FlagDraft): {},
	}
	for _, msg := range mbox.l {
		for flag := range msg.flags {
			m[flag] = struct{}{}
		}
	}
	var l []imap.Flag
	for flag := range m {
		l = append(l, flag)
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	return l
}

func (mbox *Mailbox) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	expunged := make(map[*message]struct{})
	for _, msg := range mbox.l {
		if uids != nil && !uids.Contains(msg.uid) {
			continue
		}
		if _, ok := msg.flags[canonicalFlag(imap.FlagDeleted)]; ok {
			expunged[msg] = struct{}{}
		}
	}
	mbox.expungeLocked(expunged, true)
	return nil
}

// expungeLocked removes messages. With store=true they are deleted from
// Postgres first; a message whose delete fails stays put. store=false is for
// rows that are already gone (deleted from the web inbox).
func (mbox *Mailbox) expungeLocked(expunged map[*message]struct{}, store bool) (seqNums []uint32) {
	if store {
		for msg := range expunged {
			if err := mbox.p.remove(mbox, msg); err != nil {
				mbox.p.log.Error("imap expunge failed; message kept", "mailbox", mbox.name, "uid", msg.uid, "err", err)
				delete(expunged, msg)
			}
		}
	}
	if len(expunged) == 0 {
		return nil
	}
	var filtered []*message
	for i := len(mbox.l) - 1; i >= 0; i-- {
		msg := mbox.l[i]
		if _, ok := expunged[msg]; ok {
			seqNum := uint32(i) + 1
			seqNums = append(seqNums, seqNum)
			mbox.tracker.QueueExpunge(seqNum)
		} else {
			filtered = append(filtered, msg)
		}
	}
	for i := 0; i < len(filtered)/2; i++ {
		j := len(filtered) - i - 1
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	mbox.l = filtered
	return seqNums
}

// NewView creates a per-connection view of this mailbox.
func (mbox *Mailbox) NewView() *MailboxView {
	return &MailboxView{Mailbox: mbox, tracker: mbox.tracker.NewSession()}
}

// MailboxView is a per-connection view with its own pending updates.
type MailboxView struct {
	*Mailbox
	tracker   *imapserver.SessionTracker
	searchRes imap.UIDSet
}

func (mbox *MailboxView) Close() { mbox.tracker.Close() }

func (mbox *MailboxView) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	markSeen := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}
	var err error
	mbox.forEach(numSet, func(seqNum uint32, msg *message) {
		if err != nil {
			return
		}
		if _, seen := msg.flags[canonicalFlag(imap.FlagSeen)]; markSeen && !seen {
			msg.flags[canonicalFlag(imap.FlagSeen)] = struct{}{}
			if perr := mbox.p.setFlags(mbox.Mailbox, msg); perr != nil {
				mbox.p.log.Error("imap mark seen", "uid", msg.uid, "err", perr)
			}
			mbox.Mailbox.tracker.QueueMessageFlags(seqNum, msg.uid, msg.flagList(), nil)
		}
		respWriter := w.CreateMessage(mbox.tracker.EncodeSeqNum(seqNum))
		err = msg.fetch(respWriter, options)
	})
	return err
}

func (mbox *MailboxView) Search(numKind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	mbox.staticSearchCriteria(criteria)

	var (
		data   imap.SearchData
		seqSet imap.SeqSet
		uidSet imap.UIDSet
	)
	for i, msg := range mbox.l {
		seqNum := mbox.tracker.EncodeSeqNum(uint32(i) + 1)
		if !msg.search(seqNum, criteria) {
			continue
		}
		uidSet.AddNum(msg.uid)
		var num uint32
		switch numKind {
		case imapserver.NumKindSeq:
			if seqNum == 0 {
				continue
			}
			seqSet.AddNum(seqNum)
			num = seqNum
		case imapserver.NumKindUID:
			num = uint32(msg.uid)
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if data.Max == 0 || num > data.Max {
			data.Max = num
		}
		data.Count++
	}
	switch numKind {
	case imapserver.NumKindSeq:
		data.All = seqSet
	case imapserver.NumKindUID:
		data.All = uidSet
	}
	if options.ReturnSave {
		mbox.searchRes = uidSet
	}
	return &data, nil
}

func (mbox *MailboxView) staticSearchCriteria(criteria *imap.SearchCriteria) {
	seqNums := make([]imap.SeqSet, 0, len(criteria.SeqNum))
	for _, seqSet := range criteria.SeqNum {
		switch numSet := mbox.staticNumSet(seqSet).(type) {
		case imap.SeqSet:
			seqNums = append(seqNums, numSet)
		case imap.UIDSet:
			criteria.UID = append(criteria.UID, numSet)
		}
	}
	criteria.SeqNum = seqNums
	for i, uidSet := range criteria.UID {
		criteria.UID[i] = mbox.staticNumSet(uidSet).(imap.UIDSet)
	}
	for i := range criteria.Not {
		mbox.staticSearchCriteria(&criteria.Not[i])
	}
	for i := range criteria.Or {
		for j := range criteria.Or[i] {
			mbox.staticSearchCriteria(&criteria.Or[i][j])
		}
	}
}

func (mbox *MailboxView) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	mbox.forEach(numSet, func(seqNum uint32, msg *message) {
		before := msg.flagList()
		msg.store(flags)
		if err := mbox.p.setFlags(mbox.Mailbox, msg); err != nil {
			mbox.p.log.Error("imap store flags; reverted", "uid", msg.uid, "err", err)
			msg.flags = make(map[imap.Flag]struct{})
			for _, f := range before {
				msg.flags[f] = struct{}{}
			}
			return
		}
		mbox.Mailbox.tracker.QueueMessageFlags(seqNum, msg.uid, msg.flagList(), mbox.tracker)
	})
	if !flags.Silent {
		return mbox.Fetch(w, numSet, &imap.FetchOptions{Flags: true})
	}
	return nil
}

func (mbox *MailboxView) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	return mbox.tracker.Poll(w, allowExpunge)
}

func (mbox *MailboxView) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	return mbox.tracker.Idle(w, stop)
}

func (mbox *MailboxView) forEach(numSet imap.NumSet, f func(seqNum uint32, msg *message)) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	mbox.forEachLocked(numSet, f)
}

func (mbox *MailboxView) forEachLocked(numSet imap.NumSet, f func(seqNum uint32, msg *message)) {
	numSet = mbox.staticNumSet(numSet)
	for i, msg := range mbox.l {
		seqNum := uint32(i) + 1
		var contains bool
		switch numSet := numSet.(type) {
		case imap.SeqSet:
			seqNum := mbox.tracker.EncodeSeqNum(seqNum)
			contains = seqNum != 0 && numSet.Contains(seqNum)
		case imap.UIDSet:
			contains = numSet.Contains(msg.uid)
		}
		if contains {
			f(seqNum, msg)
		}
	}
}

func (mbox *MailboxView) staticNumSet(numSet imap.NumSet) imap.NumSet {
	if imap.IsSearchRes(numSet) {
		return mbox.searchRes
	}
	switch numSet := numSet.(type) {
	case imap.SeqSet:
		max := uint32(len(mbox.l))
		for i := range numSet {
			r := &numSet[i]
			staticNumRange(&r.Start, &r.Stop, max)
		}
	case imap.UIDSet:
		max := uint32(mbox.uidNext) - 1
		for i := range numSet {
			r := &numSet[i]
			staticNumRange((*uint32)(&r.Start), (*uint32)(&r.Stop), max)
		}
	}
	return numSet
}

func staticNumRange(start, stop *uint32, max uint32) {
	dyn := false
	if *start == 0 {
		*start = max
		dyn = true
	}
	if *stop == 0 {
		*stop = max
		dyn = true
	}
	if dyn && *start > *stop {
		*start, *stop = *stop, *start
	}
}
