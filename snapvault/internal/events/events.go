// Package events defines the message contract shared across Snapvault
// services: the JetStream stream and subjects, and the event payloads
// published to them.
//
// This is the one piece of *domain* knowledge shared between services. It
// deliberately lives outside internal/platform, which holds infrastructure
// only. A service may depend on the contract it speaks; it must never depend
// on another service's internals.
package events

import "time"

// Stream and subject names for the image pipeline.
const (
	// StreamName is the JetStream stream that captures every image event.
	StreamName = "IMAGES"

	// SubjectUploaded is published when an original image has been stored.
	SubjectUploaded = "images.uploaded"

	// SubjectThumbnailed is published when a thumbnail has been generated.
	SubjectThumbnailed = "images.thumbnailed"

	// SubjectWildcard matches every image subject; the stream captures it and
	// the feed service consumes it.
	SubjectWildcard = "images.>"
)

// ImageUploaded is emitted by the images service after an original is stored
// in object storage. Consumers (thumbnails, metadata, feed) react to it.
type ImageUploaded struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	ObjectName   string    `json:"object_name"`
	ContentType  string    `json:"content_type"`
	OriginalName string    `json:"original_name"`
	Size         int64     `json:"size"`
	UploadedAt   time.Time `json:"uploaded_at"`
}

// ImageThumbnailed is emitted by the thumbnails service after a thumbnail has
// been generated and stored. The feed service uses it to attach a thumbnail to
// the corresponding feed item.
type ImageThumbnailed struct {
	ID          string    `json:"id"`
	ThumbObject string    `json:"thumb_object"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	CreatedAt   time.Time `json:"created_at"`
}
