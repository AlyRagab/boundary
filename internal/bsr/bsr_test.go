// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package bsr_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/hashicorp/boundary/internal/bsr"
	"github.com/hashicorp/boundary/internal/bsr/internal/fstest"
	"github.com/hashicorp/boundary/internal/bsr/kms"
	"github.com/hashicorp/boundary/internal/storage"
	wrapping "github.com/hashicorp/go-kms-wrapping/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func assertContainer(ctx context.Context, t *testing.T, path, state string, typ string, fs *fstest.MemContainer, keys *kms.Keys) {
	t.Helper()

	td := filepath.Join("testdata", t.Name(), state, path)

	// meta
	wantMeta, err := os.ReadFile(filepath.Join(td, string(typ)+".meta"))
	require.NoError(t, err, "unable to find test data file")
	meta, ok := fs.Files[string(typ)+".meta"]
	require.True(t, ok, "container is missing meta file")
	assert.Equal(t, string(wantMeta), meta.Buf.String())

	// summary
	wantSummary, err := os.ReadFile(filepath.Join(td, string(typ)+".summary"))
	require.NoError(t, err, "unable to find test data file")
	summary, ok := fs.Files[string(typ)+".summary"]
	require.True(t, ok, "container is missing summary file")
	assert.Equal(t, string(wantSummary), summary.Buf.String())

	// SHA256SUM checksums
	wantChecksums, err := os.ReadFile(filepath.Join(td, "SHA256SUM"))
	require.NoError(t, err, "unable to find test data file")
	checksums, ok := fs.Files["SHA256SUM"]
	require.True(t, ok, "container is missing checksums file")
	assert.Equal(t, string(wantChecksums), checksums.Buf.String())

	// SHA256SUM.sig signature file
	sig, ok := fs.Files["SHA256SUM.sig"]
	require.True(t, ok, "container is missing sig file")
	switch state {
	case "closed":
		want, err := keys.SignWithPrivKey(ctx, wantChecksums)
		require.NoError(t, err)

		got := &wrapping.SigInfo{}
		err = proto.Unmarshal(sig.Buf.Bytes(), got)
		require.NoError(t, err)

		assert.Empty(t,
			cmp.Diff(
				want,
				got,
				cmpopts.IgnoreUnexported(wrapping.SigInfo{}, wrapping.KeyInfo{}),
			),
		)
	default:
		assert.Equal(t, "", sig.Buf.String())
	}

	// journal
	wantJournal, err := os.ReadFile(filepath.Join(td, ".journal"))
	require.NoError(t, err, "unable to find test data file")
	journal, ok := fs.Files[".journal"]
	require.True(t, ok, "container is missing journal file")
	assert.Equal(t, string(wantJournal), journal.Buf.String())
}

type connection struct {
	mem  *fstest.MemContainer
	conn *bsr.Connection
	id   string

	channels []*channel
	files    []*file
}

type channel struct {
	mem     *fstest.MemContainer
	channel *bsr.Channel
	id      string

	files []*file
}

type file struct {
	mem  *fstest.MemFile
	file io.Writer
}

type createConn struct {
	id       string
	channels []createChannel
	files    []createFile
}

type createChannel struct {
	id    string
	files []createFile
}

type createFile struct {
	typ string
	dir bsr.Direction
}

func TestBsr(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		id    string
		opts  []bsr.Option
		c     *fstest.MemFS
		keys  *kms.Keys
		conns []createConn
	}{
		{
			"session_not_multiplexed",
			"session_123456789",
			[]bsr.Option{},
			fstest.NewMemFS(),
			func() *kms.Keys {
				keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session_123456789")
				require.NoError(t, err)
				return keys
			}(),
			[]createConn{
				{
					"conn_1",
					nil,
					[]createFile{
						{"messages", bsr.Inbound},
						{"messages", bsr.Outbound},
						{"requests", bsr.Inbound},
						{"requests", bsr.Outbound},
					},
				},
			},
		},
		{
			"session_multiplexed",
			"session_123456789",
			[]bsr.Option{bsr.WithSupportsMultiplex(true)},
			fstest.NewMemFS(),
			func() *kms.Keys {
				keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session_123456789")
				require.NoError(t, err)
				return keys
			}(),
			[]createConn{
				{
					"conn_1",
					[]createChannel{
						{
							"chan_1",
							[]createFile{
								{"messages", bsr.Inbound},
								{"requests", bsr.Inbound},
								{"messages", bsr.Outbound},
								{"requests", bsr.Outbound},
							},
						},
					},
					[]createFile{
						{"requests", bsr.Inbound},
						{"requests", bsr.Outbound},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: tc.id}, tc.c, tc.keys, tc.opts...)
			require.NoError(t, err)
			require.NotNil(t, s)

			sContainer, ok := tc.c.Containers[tc.id+".bsr"]
			require.True(t, ok)

			assertContainer(ctx, t, "", "opened", "session", sContainer, tc.keys)

			createdConnections := make([]*connection, 0)

			// create all the things
			for _, conn := range tc.conns {
				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: conn.id})
				require.NoError(t, err)
				require.NotNil(t, c)

				cContainer, ok := sContainer.Sub[conn.id+".connection"]
				require.True(t, ok)

				assertContainer(ctx, t, conn.id, "opened", "connection", cContainer, tc.keys)

				ff := make([]*file, 0, len(conn.files))
				for _, f := range conn.files {
					var w io.Writer
					var err error
					switch f.typ {
					case "messages":
						w, err = c.NewMessagesWriter(ctx, f.dir)
					case "requests":
						w, err = c.NewRequestsWriter(ctx, f.dir)
					}
					require.NoError(t, err)

					fname := fmt.Sprintf("%s-%s.data", f.typ, f.dir.String())
					memf, ok := cContainer.Files[fname]
					require.True(t, ok, "file %s not in container %s", fname, cContainer.Name)

					require.NoError(t, err)
					ff = append(ff, &file{
						mem:  memf,
						file: w,
					})
				}

				createdChannels := make([]*channel, 0, len(conn.channels))
				for _, chann := range conn.channels {
					ch, err := c.NewChannel(ctx, &bsr.ChannelMeta{Id: chann.id})
					require.NoError(t, err)
					require.NotNil(t, ch)

					chContainer, ok := cContainer.Sub[chann.id+".channel"]
					require.True(t, ok)

					assertContainer(ctx, t, filepath.Join(conn.id, chann.id), "opened", "channel", chContainer, tc.keys)

					ff := make([]*file, 0, len(chann.files))
					for _, f := range chann.files {
						var w io.Writer
						var err error
						switch f.typ {
						case "messages":
							w, err = ch.NewMessagesWriter(ctx, f.dir)
						case "requests":
							w, err = ch.NewRequestsWriter(ctx, f.dir)
						}
						require.NoError(t, err)

						fname := fmt.Sprintf("%s-%s.data", f.typ, f.dir.String())
						memf, ok := chContainer.Files[fname]
						require.True(t, ok, "file %s not in container %s", fname, chContainer.Name)

						require.NoError(t, err)
						ff = append(ff, &file{
							mem:  memf,
							file: w,
						})
					}
					createdChannels = append(createdChannels, &channel{
						mem:     chContainer,
						channel: ch,
						id:      chann.id,
						files:   ff,
					})
				}
				createdConnections = append(createdConnections, &connection{
					mem:      cContainer,
					conn:     c,
					id:       conn.id,
					channels: createdChannels,
					files:    ff,
				})
			}

			// now close all the things that where created.
			for _, conn := range createdConnections {
				for _, channel := range conn.channels {
					for _, f := range channel.files {
						v, ok := f.file.(io.Closer)
						require.True(t, ok, "file is not a io.Closer")
						err = v.Close()
						require.NoError(t, err)
					}
					err = channel.channel.Close(ctx)
					require.NoError(t, err)

					assertContainer(ctx, t, filepath.Join(conn.id, channel.id), "closed", "channel", channel.mem, tc.keys)
				}

				for _, f := range conn.files {
					v, ok := f.file.(io.Closer)
					require.True(t, ok, "file is not a io.Closer")
					err = v.Close()
					require.NoError(t, err)
				}

				err = conn.conn.Close(ctx)
				require.NoError(t, err)
				assertContainer(ctx, t, conn.id, "closed", "connection", conn.mem, tc.keys)
			}

			err = s.Close(ctx)
			require.NoError(t, err)

			assertContainer(ctx, t, "", "closed", "session", sContainer, tc.keys)
		})
	}
}

func TestNewSessionErrors(t *testing.T) {
	ctx := context.Background()

	keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session")
	require.NoError(t, err)

	cases := []struct {
		name      string
		meta      *bsr.SessionMeta
		f         storage.FS
		keys      *kms.Keys
		wantError error
	}{
		{
			"nil-meta",
			nil,
			&fstest.MemFS{},
			keys,
			errors.New("bsr.NewSession: missing session meta: invalid parameter"),
		},
		{
			"empty-session-id",
			&bsr.SessionMeta{Id: ""},
			&fstest.MemFS{},
			keys,
			errors.New("bsr.NewSession: missing session id: invalid parameter"),
		},
		{
			"nil-fs",
			&bsr.SessionMeta{Id: "session"},
			nil,
			keys,
			errors.New("bsr.NewSession: missing storage fs: invalid parameter"),
		},
		{
			"nil-keys",
			&bsr.SessionMeta{Id: "session"},
			&fstest.MemFS{},
			nil,
			errors.New("bsr.NewSession: missing kms keys: invalid parameter"),
		},
		{
			"fs-new-error",
			&bsr.SessionMeta{Id: "session"},
			fstest.NewMemFS(fstest.WithNewFunc(func(_ context.Context, _ string) (storage.Container, error) {
				return nil, errors.New("fs new error")
			})),
			keys,
			errors.New("fs new error"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bsr.NewSession(ctx, tc.meta, tc.f, tc.keys)
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantError.Error())
		})
	}
}

func TestNewConnectionErrors(t *testing.T) {
	ctx := context.Background()

	keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session")
	require.NoError(t, err)

	cases := []struct {
		name      string
		session   *bsr.Session
		meta      *bsr.ConnectionMeta
		wantError error
	}{
		{
			"nil-meta",
			func() *bsr.Session {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys)
				require.NoError(t, err)
				return s
			}(),
			nil,
			errors.New("bsr.(Session).NewConnection: missing connection meta: invalid parameter"),
		},
		{
			"empty-connection-id",
			func() *bsr.Session {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys)
				require.NoError(t, err)
				return s
			}(),
			&bsr.ConnectionMeta{Id: ""},
			errors.New("bsr.(Session).NewConnection: missing connection id: invalid parameter"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.session.NewConnection(ctx, tc.meta)
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantError.Error())
		})
	}
}

func TestNewChannelErrors(t *testing.T) {
	ctx := context.Background()

	keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session")
	require.NoError(t, err)

	cases := []struct {
		name       string
		connection *bsr.Connection
		meta       *bsr.ChannelMeta
		wantError  error
	}{
		{
			"nil-meta",
			func() *bsr.Connection {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys, bsr.WithSupportsMultiplex(true))
				require.NoError(t, err)

				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: "connection"})
				require.NoError(t, err)
				return c
			}(),
			nil,
			errors.New("bsr.(Connection).NewChannel: missing channel meta: invalid parameter"),
		},
		{
			"empty-connection-id",
			func() *bsr.Connection {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys, bsr.WithSupportsMultiplex(true))
				require.NoError(t, err)

				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: "connection"})
				require.NoError(t, err)
				return c
			}(),
			&bsr.ChannelMeta{Id: ""},
			errors.New("bsr.(Connection).NewChannel: missing channel id: invalid parameter"),
		},
		{
			"not-multiplexed",
			func() *bsr.Connection {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys, bsr.WithSupportsMultiplex(false))
				require.NoError(t, err)

				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: "connection"})
				require.NoError(t, err)
				return c
			}(),
			&bsr.ChannelMeta{Id: ""},
			errors.New("bsr.(Connection).NewChannel: connection cannot make channels: not supported by protocol"),
		},
		{
			"not-multiplexed-default",
			func() *bsr.Connection {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys)
				require.NoError(t, err)

				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: "connection"})
				require.NoError(t, err)
				return c
			}(),
			&bsr.ChannelMeta{Id: ""},
			errors.New("bsr.(Connection).NewChannel: connection cannot make channels: not supported by protocol"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.connection.NewChannel(ctx, tc.meta)
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantError.Error())
		})
	}
}

func TestChannelNewMessagesWriterErrors(t *testing.T) {
	ctx := context.Background()

	keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session")
	require.NoError(t, err)

	cases := []struct {
		name      string
		channel   *bsr.Channel
		dir       bsr.Direction
		wantError error
	}{
		{
			"invalid-dir",
			func() *bsr.Channel {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys, bsr.WithSupportsMultiplex(true))
				require.NoError(t, err)

				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: "connection"})
				require.NoError(t, err)

				ch, err := c.NewChannel(ctx, &bsr.ChannelMeta{Id: "channel"})
				require.NoError(t, err)
				return ch
			}(),
			bsr.Direction(uint8(255)),
			errors.New("bsr.(Channel).NewMessagesWriter: invalid direction: invalid parameter"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.channel.NewMessagesWriter(ctx, tc.dir)
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantError.Error())
		})
	}
}

func TestChannelNewRequestsWriterErrors(t *testing.T) {
	ctx := context.Background()

	keys, err := kms.CreateKeys(ctx, kms.TestWrapper(t), "session")
	require.NoError(t, err)

	cases := []struct {
		name      string
		channel   *bsr.Channel
		dir       bsr.Direction
		wantError error
	}{
		{
			"invalid-dir",
			func() *bsr.Channel {
				s, err := bsr.NewSession(ctx, &bsr.SessionMeta{Id: "session"}, &fstest.MemFS{}, keys, bsr.WithSupportsMultiplex(true))
				require.NoError(t, err)

				c, err := s.NewConnection(ctx, &bsr.ConnectionMeta{Id: "connection"})
				require.NoError(t, err)

				ch, err := c.NewChannel(ctx, &bsr.ChannelMeta{Id: "channel"})
				require.NoError(t, err)
				return ch
			}(),
			bsr.Direction(uint8(255)),
			errors.New("bsr.(Channel).NewRequestsWriter: invalid direction: invalid parameter"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.channel.NewRequestsWriter(ctx, tc.dir)
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantError.Error())
		})
	}
}