package drive

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/rclone/rclone/fs"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/driveactivity/v2"
)

const (
	moveQuery = "detail.action_detail_case:MOVE"
)

type (
	parent struct {
		ID   string
		Name string
	}

	entryType struct {
		path      string
		entryType fs.EntryType
	}
)

var (
	consolidationStrategy = driveactivity.ConsolidationStrategy{
		None: new(driveactivity.NoConsolidation),
	}
)

func parseItemID(itemName string) string {
	return strings.Replace(itemName, "items/", "", -1)
}

func createItemQueryRequest(itemID, filter, pageToken string) *driveactivity.
	QueryDriveActivityRequest {
	return &driveactivity.QueryDriveActivityRequest{
		PageSize:              1,
		ItemName:              fmt.Sprintf("items/%s", itemID),
		ConsolidationStrategy: &consolidationStrategy,
		Filter:                filter,
		PageToken:             pageToken,
	}
}

func (f *Fs) getItemParents(ctx context.Context, itemID string) (parents []*parent) {
	var (
		err          error
		moveResponse *driveactivity.QueryDriveActivityResponse
	)
	itemMoveQuery := createItemQueryRequest(itemID, moveQuery, "")
	err = f.pacer.Call(func() (bool, error) {
		moveResponse, err = f.activitySvc.Activity.Query(itemMoveQuery).Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		fs.Errorf(itemID, "Unable to retrieve list of moves: %v", err)
		return nil
	}
	fs.Debugf(itemID, "Retrieved Moves : %d\n", len(moveResponse.Activities))

	for _, activity := range moveResponse.Activities {
		for _, action := range activity.Actions {
			if action.Detail.Move != nil {
				for _, addedParent := range action.Detail.Move.AddedParents {
					if addedParent.DriveItem == nil {
						fs.Errorf(itemID, "Invalid Added Parent on Move Activity: %v\n",
							addedParent)
						continue
					}
					parentItem := addedParent.DriveItem
					parents = append(parents, &parent{ID: parseItemID(parentItem.Name),
						Name: parentItem.Title})
				}
				for _, removedParent := range action.Detail.Move.RemovedParents {
					if removedParent.DriveItem == nil {
						fs.Errorf(itemID, "Invalid Removed Parent on Move Activity: %v\n",
							removedParent)
						continue
					}
					parentItem := removedParent.DriveItem
					parents = append(parents, &parent{ID: parseItemID(parentItem.Name),
						Name: parentItem.Title})
				}
			}
		}
	}

	return
}

func (f *Fs) changeNotifyStartPageToken(ctx context.Context) (pageToken string, err error) {
	var startPageToken *drive.StartPageToken
	err = f.pacer.Call(func() (bool, error) {
		changes := f.svc.Changes.GetStartPageToken().SupportsAllDrives(true)
		if f.isTeamDrive {
			changes.DriveId(f.opt.TeamDriveID)
		}
		startPageToken, err = changes.Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return
	}
	return startPageToken.StartPageToken, nil
}

func (f *Fs) changeNotifyRunner(ctx context.Context, notifyFunc func(string, fs.EntryType),
	startPageToken string) (newStartPageToken string, err error) {

	var (
		pageToken    string
		pathsToClear []entryType
	)

	pageToken = startPageToken
	for {
		var changeList *drive.ChangeList

		err = f.pacer.Call(func() (bool, error) {
			changesCall := f.svc.Changes.List(pageToken).
				Fields("nextPageToken,newStartPageToken,changes(fileId,file(name,parents,mimeType))")
			changesCall.SupportsAllDrives(true)
			changesCall.IncludeItemsFromAllDrives(true)
			if f.opt.ListChunk > 0 {
				changesCall.PageSize(f.opt.ListChunk)
			}
			if f.isTeamDrive {
				changesCall.DriveId(f.opt.TeamDriveID)
			}
			// If using appDataFolder then need to add Spaces
			if f.rootFolderID == "appDataFolder" {
				changesCall.Spaces("appDataFolder")
			}
			changeList, err = changesCall.Context(ctx).Do()
			return f.shouldRetry(ctx, err)
		})
		if err != nil {
			return
		}
		fs.Infof(f, "Retrieved Changes : %d\n", len(changeList.Changes))

		pathsToClear = []entryType{}

		for _, change := range changeList.Changes {
			// Invalidate the previous path. Currently doesn't work for Files
			if cachedPath, ok := f.dirCache.GetInv(change.FileId); ok {
				if change.File != nil && change.File.MimeType != driveFolderType {
					pathsToClear = append(pathsToClear, entryType{
						path: cachedPath, entryType: fs.EntryObject})
				} else {
					pathsToClear = append(pathsToClear, entryType{
						path: cachedPath, entryType: fs.EntryDirectory})
				}
			}

			if change.File != nil {
				change.File.Name = f.opt.Enc.ToStandardName(change.File.Name)
				changeType := fs.EntryDirectory
				if change.File.MimeType != driveFolderType {
					changeType = fs.EntryObject
				}
				if len(change.File.Parents) > 0 {
					for _, parent := range change.File.Parents {
						// translate the parent dir of this object
						if parentPath, ok := f.dirCache.GetInv(parent); ok {
							// and append the drive file name to compute the full file name
							newPath := path.Join(parentPath, change.File.Name)
							// this will now clear the actual file too
							pathsToClear = append(pathsToClear, entryType{
								path: newPath, entryType: changeType})
						}
					}
				} else { // a true root object that is changed
					pathsToClear = append(pathsToClear, entryType{
						path: change.File.Name, entryType: changeType})
				}
			}
		}

		visitedPaths := make(map[string]struct{})
		for _, entry := range pathsToClear {
			if _, ok := visitedPaths[entry.path]; ok {
				continue
			}
			visitedPaths[entry.path] = struct{}{}
			fs.Debugf(f, "Clearing Parent : %s (%v)", entry.path, entry.entryType)
			notifyFunc(entry.path, entry.entryType)
		}

		switch {
		case changeList.NewStartPageToken != "":
			return changeList.NewStartPageToken, nil
		case changeList.NextPageToken != "":
			pageToken = changeList.NextPageToken
		default:
			return
		}
	}
}
