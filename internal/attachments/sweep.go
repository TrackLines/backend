package attachments

import "github.com/tracklines/backend/retention"

// Sweeper deletes files from UploadThing once their attachment is gone, however it went (TL-206).
// It lives in package retention, which the automation service runs nightly (TL-431).
type Sweeper = retention.Sweeper

// UploadThing deletes stored files (retention.UploadThing).
type UploadThing = retention.UploadThing

// KeepDays is how long a deleted file waits before it's deleted from storage (TL-256).
const KeepDays = retention.KeepDays
