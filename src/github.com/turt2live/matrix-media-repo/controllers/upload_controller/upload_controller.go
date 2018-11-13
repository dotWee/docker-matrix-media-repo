package upload_controller

import (
	"context"
	"io"
	"os"
	"strconv"

	"github.com/pkg/errors"
	"github.com/ryanuber/go-glob"
	"github.com/sirupsen/logrus"
	"github.com/turt2live/matrix-media-repo/common"
	"github.com/turt2live/matrix-media-repo/common/config"
	"github.com/turt2live/matrix-media-repo/storage"
	"github.com/turt2live/matrix-media-repo/types"
	"github.com/turt2live/matrix-media-repo/util"
)

const NoApplicableUploadUser = ""

func IsRequestTooLarge(contentLength int64, contentLengthHeader string) bool {
	if config.Get().Uploads.MaxSizeBytes <= 0 {
		return false
	}
	if contentLength >= 0 {
		return contentLength > config.Get().Uploads.MaxSizeBytes
	}
	if contentLengthHeader != "" {
		parsed, err := strconv.ParseInt(contentLengthHeader, 10, 64)
		if err != nil {
			logrus.Warn("Invalid content length header given; assuming too large. Value received: " + contentLengthHeader)
			return true // Invalid header
		}

		return parsed > config.Get().Uploads.MaxSizeBytes
	}

	return false // We can only assume
}

func UploadMedia(contents io.ReadCloser, contentType string, filename string, userId string, origin string, ctx context.Context, log *logrus.Entry) (*types.Media, error) {
	defer contents.Close()

	var data io.Reader
	if config.Get().Uploads.MaxSizeBytes > 0 {
		data = io.LimitReader(contents, config.Get().Uploads.MaxSizeBytes)
	} else {
		data = contents
	}

	mediaId, err := util.GenerateRandomString(64)
	if err != nil {
		return nil, err
	}

	return StoreDirect(data, contentType, filename, userId, origin, mediaId, ctx, log)
}

func trackUploadAsLastAccess(ctx context.Context, log *logrus.Entry, media *types.Media) {
	err := storage.GetDatabase().GetMetadataStore(ctx, log).UpsertLastAccess(media.Sha256Hash, util.NowMillis())
	if err != nil {
		logrus.Warn("Failed to upsert the last access time: ", err)
	}
}

func IsAllowed(contentType string, reportedContentType string, userId string, log *logrus.Entry) (bool) {
	allowed := false
	userMatched := false

	if userId != NoApplicableUploadUser {
		for user, userExcl := range config.Get().Uploads.PerUserExclusions {
			if glob.Glob(user, userId) {
				if !userMatched {
					log.Info("Per-user allowed types policy found for " + userId)
					userMatched = true
				}
				for _, exclType := range userExcl {
					if glob.Glob(exclType, contentType) {
						allowed = true
						log.Info("Content type " + contentType + " (reported as " + reportedContentType + ") is allowed due to a per-user policy for " + userId)
						break
					}
				}
			}

			if allowed {
				break
			}
		}
	}

	if !userMatched && !allowed {
		log.Info("Checking general allowed types due to no matching per-user policy")
		for _, allowedType := range config.Get().Uploads.AllowedTypes {
			if glob.Glob(allowedType, contentType) {
				allowed = true
				break
			}
		}

		if len(config.Get().Uploads.AllowedTypes) == 0 {
			allowed = true
		}
	}

	return allowed
}

func StoreDirect(contents io.Reader, contentType string, filename string, userId string, origin string, mediaId string, ctx context.Context, log *logrus.Entry) (*types.Media, error) {
	datastore, location, err := storage.PersistFile(contents, ctx, log)
	if err != nil {
		return nil, err
	}

	fileLocation := datastore.ResolveFilePath(location)

	fileMime, err := util.GetMimeType(fileLocation)
	if err != nil {
		log.Error("Error while checking content type of file: ", err.Error())
		os.Remove(fileLocation) // delete temp file
		return nil, err
	}

	allowed := IsAllowed(fileMime, contentType, userId, log)
	if !allowed {
		log.Warn("Content type " + fileMime + " (reported as " + contentType + ") is not allowed to be uploaded")

		os.Remove(fileLocation) // delete temp file
		return nil, common.ErrMediaNotAllowed
	}

	hash, err := storage.GetFileHash(fileLocation)
	if err != nil {
		os.Remove(fileLocation) // delete temp file
		return nil, err
	}

	db := storage.GetDatabase().GetMediaStore(ctx, log)
	records, err := db.GetByHash(hash)
	if err != nil {
		os.Remove(fileLocation) // delete temp file
		return nil, err
	}

	if len(records) > 0 {
		log.Info("Duplicate media for hash ", hash)

		// If the user is a real user (ie: actually uploaded media), then we'll see if there's
		// an exact duplicate that we can return. Otherwise we'll just pick the first record and
		// clone that.
		if userId != NoApplicableUploadUser {
			for _, record := range records {
				if record.UserId == userId && record.Origin == origin && record.ContentType == contentType {
					log.Info("User has already uploaded this media before - returning unaltered media record")
					os.Remove(fileLocation) // delete temp file
					trackUploadAsLastAccess(ctx, log, record)
					return record, nil
				}
			}
		}

		// We'll use the location from the first record
		media := records[0]
		media.Origin = origin
		media.MediaId = mediaId
		media.UserId = userId
		media.UploadName = filename
		media.ContentType = contentType
		media.CreationTs = util.NowMillis()

		err = db.Insert(media)
		if err != nil {
			os.Remove(fileLocation) // delete temp file
			return nil, err
		}

		// If the media's file exists, we'll delete the temp file
		// If the media's file doesn't exist, we'll move the temp file to where the media expects it to be
		targetPath, err2 := storage.ResolveMediaLocation(ctx, log, media.DatastoreId, media.Location)
		if err2 != nil {
			return nil, err2
		}
		exists, err := util.FileExists(targetPath)
		if err != nil || !exists {
			// We'll assume an error means it doesn't exist
			os.Rename(fileLocation, targetPath)
		} else {
			os.Remove(fileLocation)
		}

		trackUploadAsLastAccess(ctx, log, media)
		return media, nil
	}

	// The media doesn't already exist - save it as new

	fileSize, err := util.FileSize(fileLocation)
	if err != nil {
		os.Remove(fileLocation) // delete temp file
		return nil, err
	}

	if fileSize <= 0 {
		return nil, errors.New("file has no contents")
	}

	log.Info("Persisting new media record")

	media := &types.Media{
		Origin:      origin,
		MediaId:     mediaId,
		UploadName:  filename,
		ContentType: contentType,
		UserId:      userId,
		Sha256Hash:  hash,
		SizeBytes:   fileSize,
		DatastoreId: datastore.DatastoreId,
		Location:    location,
		CreationTs:  util.NowMillis(),
	}

	err = db.Insert(media)
	if err != nil {
		os.Remove(fileLocation) // delete temp file
		return nil, err
	}

	trackUploadAsLastAccess(ctx, log, media)
	return media, nil
}
