package s3

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"
)

func TestS3EntryIdentity(t *testing.T) {
	ctx := context.Background()
	f := &Fs{}
	for _, tc := range []struct {
		name      string
		etag      *string
		head      bool
		want      uint64
		different bool
	}{
		{"ListingQuoted", aws.String("\"d41d8cd98f00b204e9800998ecf8427e\""), false, 16826061031705674756, false},
		{"HeadUnquoted", aws.String("d41d8cd98f00b204e9800998ecf8427e"), true, 16826061031705674756, false},
		{"Multipart", aws.String("\"9b2cf535f27731c974343645a3985328-3\""), false, 13239831829779851959, false},
		{"Different", aws.String("\"0cc175b9c0f1b6a831c399e269772661\""), false, 16826061031705674756, true},
		{"Empty", nil, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o *Object
			if tc.head {
				o = &Object{fs: f, remote: "a"}
				o.setMetaData(&s3.HeadObjectOutput{
					ETag:         tc.etag,
					LastModified: aws.Time(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)),
				})
			} else {
				obj, err := f.newObjectWithInfo(ctx, "a", &types.Object{
					ETag:         tc.etag,
					LastModified: aws.Time(time.Date(2024, 1, 2, 3, 4, 5, 123e6, time.UTC)),
				}, nil)
				require.NoError(t, err)
				o = obj.(*Object)
			}
			if tc.different {
				require.NotEqual(t, tc.want, s3Identity(deref(tc.etag)))
				require.NotEqual(t, tc.want, o.EntryIdentity())
			} else {
				require.Equal(t, tc.want, s3Identity(deref(tc.etag)))
				require.Equal(t, tc.want, o.EntryIdentity())
			}
		})
	}
}
