package drive

//const (
//	globalQuery = "time >= \"%s\" AND time < \"%s\""
//)
//
//func createGlobalQueryRequest(rootFolderID, filter, pageToken string) *driveactivity.
//	QueryDriveActivityRequest {
//	return &driveactivity.QueryDriveActivityRequest{
//		PageSize:              10,
//		AncestorName:          fmt.Sprintf("items/%s", rootFolderID),
//		ConsolidationStrategy: &consolidationStrategy,
//		Filter:                filter,
//		PageToken:             pageToken,
//	}
//}
//
//func addParent(parents map[string]string, parent *parent) map[string]string {
//	if parent == nil {
//		return parents
//	}
//	if _, ok := parents[parent.ID]; !ok {
//		parents[parent.ID] = parent.Name
//	}
//	return parents
//}
//
//// Returns the type of a target and an associated title.
//func parseTarget(target *driveactivity.Target) *driveactivity.DriveItem {
//	return target.DriveItem
//}
//
//// Returns the first target for a list of targets.
//func getTargetInfo(targets []*driveactivity.Target) (targetInfo *driveactivity.DriveItem) {
//	for _, target := range targets {
//		if targetInfo = parseTarget(target); targetInfo != nil {
//			return
//		}
//	}
//	return nil
//}
//
//func (f *Fs) getParent(ctx context.Context, target *driveactivity.DriveItem) *parent {
//	var (
//		err          error
//		moveResponse *driveactivity.QueryDriveActivityResponse
//	)
//	targetTitle := target.Title
//	itemMoveQuery := createItemQueryRequest(parseItemID(target.Name), moveQuery, "")
//	err = f.pacer.Call(func() (bool, error) {
//		moveResponse, err = f.activitySvc.Activity.Query(itemMoveQuery).Context(ctx).Do()
//		return f.shouldRetry(ctx, err)
//	})
//	if err != nil {
//		fs.Errorf(targetTitle, "Unable to retrieve list of moves: %v", err)
//		return nil
//	}
//	fs.Debugf(targetTitle, "Retrieved Moves : %d\n", len(moveResponse.Activities))
//
//	for _, activity := range moveResponse.Activities {
//		for _, action := range activity.Actions {
//			if action.Detail.Move != nil {
//				for _, addedParent := range action.Detail.Move.AddedParents {
//					parentItem := addedParent.DriveItem
//					return &parent{ID: parseItemID(parentItem.Name), Name: parentItem.Title}
//				}
//			}
//		}
//	}
//	return nil
//}
//
//func (f *Fs) parseActivityActions(ctx context.Context, driveTarget *driveactivity.DriveItem,
//	actions []*driveactivity.Action) (invalidatedParents map[string]string) {
//
//	var (
//		invalidatedParent *parent
//	)
//	invalidatedParents = make(map[string]string)
//	for _, action := range actions {
//		target := driveTarget
//		if action.Target != nil && parseTarget(action.Target) != nil {
//			target = parseTarget(action.Target)
//		}
//		targetTitle := target.Title
//		if action.Detail.Delete != nil {
//			fs.Debugf(targetTitle, "Deleted")
//			invalidatedParent = f.getParent(ctx, target)
//			invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//		}
//		if action.Detail.Rename != nil {
//			fs.Debugf(targetTitle, "Renamed")
//			invalidatedParent = f.getParent(ctx, target)
//			invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//		}
//		if action.Detail.Restore != nil {
//			fs.Debugf(targetTitle, "Restored")
//			invalidatedParent = f.getParent(ctx, target)
//			invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//		}
//		/* TODO: Verify if this is needed
//		if action.Detail.Edit != nil {
//			fs.Debugf(targetTitle, "Edited")
//			invalidatedParent = f.getParent(ctx, target)
//			invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//		}
//		*/
//		/* TODO: Verify if this is needed
//		if action.Detail.PermissionChange != nil {
//			fs.Debugf(targetTitle, "Permissions Changed")
//			invalidatedParent = f.getParent(ctx, target)
//			invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//		}
//		*/
//		if action.Detail.Move != nil {
//			fs.Debugf(targetTitle, "Moved")
//			moveAction := action.Detail.Move
//			for _, addedParent := range moveAction.AddedParents {
//				if addedParent.DriveItem == nil {
//					fs.Errorf(targetTitle, "Invalid Added Parent on Move Activity: %v\n",
//						addedParent)
//					continue
//				}
//				parentItem := addedParent.DriveItem
//				invalidatedParent = &parent{
//					ID: parseItemID(parentItem.Name), Name: parentItem.Title}
//				invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//			}
//			for _, removedParent := range moveAction.RemovedParents {
//				if removedParent.DriveItem == nil {
//					fs.Errorf(targetTitle, "Invalid Removed Parent on Move Activity: %v\n",
//						removedParent)
//					continue
//				}
//				parentItem := removedParent.DriveItem
//				invalidatedParent = &parent{
//					ID: parseItemID(parentItem.Name), Name: parentItem.Title}
//				invalidatedParents = addParent(invalidatedParents, invalidatedParent)
//			}
//		}
//		for _, invalidatedParent := range invalidatedParents {
//			fs.Debugf(targetTitle, "Invalidated Parent : %s", invalidatedParent)
//		}
//	}
//	return
//}
//
//func (f *Fs) parseActivity(ctx context.Context, activity *driveactivity.DriveActivity) map[string]string {
//	target := getTargetInfo(activity.Targets)
//	return f.parseActivityActions(ctx, target, activity.Actions)
//}
//
//func (f *Fs) changeActivityRunner(ctx context.Context, fromTime,
//	toTime time.Time, notifyFunc func(string, fs.EntryType)) {
//
//	var (
//		err                error
//		pageToken          string
//		pathsToClear       []entryType
//		invalidatedParents map[string]string
//		activityQuery      *driveactivity.QueryDriveActivityRequest
//		activityResponse   *driveactivity.QueryDriveActivityResponse
//	)
//
//	filter := fmt.Sprintf(globalQuery,
//		fromTime.Format(time.RFC3339), toTime.Format(time.RFC3339))
//
//	for {
//		activityQuery = createGlobalQueryRequest(f.rootFolderID, filter, pageToken)
//
//		err = f.pacer.Call(func() (bool, error) {
//			activityResponse, err = f.activitySvc.Activity.Query(activityQuery).Context(ctx).Do()
//			return f.shouldRetry(ctx, err)
//		})
//		if err != nil {
//			fs.Errorf(f, "Unable to retrieve list of activities: %v", err)
//			return
//		}
//		fs.Infof(f, "Retrieved Activities : %d\n", len(activityResponse.Activities))
//
//		for _, activity := range activityResponse.Activities {
//			invalidatedParents = f.parseActivity(ctx, activity)
//
//			for parentID := range invalidatedParents {
//				// translate the path of this dir
//				if parentPath, ok := f.dirCache.GetInv(parentID); ok {
//					pathsToClear = append(pathsToClear, entryType{
//						path: parentPath, entryType: fs.EntryDirectory})
//				}
//			}
//		}
//
//		visitedPaths := make(map[string]struct{})
//		for _, entry := range pathsToClear {
//			if _, ok := visitedPaths[entry.path]; ok {
//				continue
//			}
//			visitedPaths[entry.path] = struct{}{}
//			fs.Debugf(f, "Clearing Parent : %s (%v)", entry.path, entry.entryType)
//			notifyFunc(entry.path, entry.entryType)
//		}
//
//		pageToken = activityResponse.NextPageToken
//		if pageToken == "" {
//			break
//		}
//	}
//}
