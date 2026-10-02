package maildata

type Attachment struct {
	FileName  string `json:"file_name"`
	MIMEType  string `json:"mime_type"`
	Size      int    `json:"size"`
	SHA256    string `json:"sha256"`
	LocalPath string `json:"local_path"`
}

type Record struct {
	Source       string       `json:"source"`
	Account      string       `json:"account"`
	MessageID    string       `json:"message_id"`
	ThreadID     string       `json:"thread_id,omitempty"`
	HistoryID    string       `json:"history_id,omitempty"`
	Folder       string       `json:"folder,omitempty"`
	UID          uint32       `json:"uid,omitempty"`
	InternalDate string       `json:"internal_date,omitempty"`
	Date         string       `json:"date"`
	From         string       `json:"from"`
	To           string       `json:"to"`
	Subject      string       `json:"subject"`
	Snippet      string       `json:"snippet"`
	BodyText     string       `json:"body_text"`
	Attachments  []Attachment `json:"attachments"`
}
