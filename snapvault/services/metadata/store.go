package main

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// ImageMeta is the document stored per image. It carries both bson tags (for
// MongoDB) and json tags (for the read API).
type ImageMeta struct {
	ID           string    `bson:"_id" json:"id"`
	Owner        string    `bson:"owner" json:"owner"`
	ContentType  string    `bson:"content_type" json:"content_type"`
	OriginalName string    `bson:"original_name" json:"original_name"`
	Size         int64     `bson:"size" json:"size"`
	Tags         []string  `bson:"tags,omitempty" json:"tags,omitempty"`
	UploadedAt   time.Time `bson:"uploaded_at" json:"uploaded_at"`
}

type metaStore struct {
	client *mongo.Client
	coll   *mongo.Collection
}

// newMetaStore connects to MongoDB and returns a store over the "images"
// collection. Note the v2 driver's Connect takes no context.
func newMetaStore(ctx context.Context, uri, dbName string) (*metaStore, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	coll := client.Database(dbName).Collection("images")
	return &metaStore{client: client, coll: coll}, nil
}

func (s *metaStore) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

func (s *metaStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx, readpref.Primary())
}

// Upsert writes m keyed by its ID. It sets only the mutable fields (never
// _id), so it is idempotent and safe under at-least-once delivery: replaying
// the same event simply rewrites the same document.
func (s *metaStore) Upsert(ctx context.Context, m ImageMeta) error {
	filter := bson.D{{Key: "_id", Value: m.ID}}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "owner", Value: m.Owner},
		{Key: "content_type", Value: m.ContentType},
		{Key: "original_name", Value: m.OriginalName},
		{Key: "size", Value: m.Size},
		{Key: "tags", Value: m.Tags},
		{Key: "uploaded_at", Value: m.UploadedAt},
	}}}
	_, err := s.coll.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	return err
}

// Get returns the metadata for id, or mongo.ErrNoDocuments if absent.
func (s *metaStore) Get(ctx context.Context, id string) (ImageMeta, error) {
	var m ImageMeta
	err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&m)
	return m, err
}

// ByTag returns all images carrying the given tag.
func (s *metaStore) ByTag(ctx context.Context, tag string) ([]ImageMeta, error) {
	cur, err := s.coll.Find(ctx, bson.D{{Key: "tags", Value: tag}})
	if err != nil {
		return nil, err
	}
	var out []ImageMeta
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
