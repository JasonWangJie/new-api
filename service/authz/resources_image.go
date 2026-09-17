package authz

const (
	ActionManage            = "manage"
	ResourceAsyncImageTask  = "async_image_task"
	ResourceImageModeration = "image_moderation"
	ResourceImageConfig     = "image_config"
)

var (
	AsyncImageTaskRead    = Permission{Resource: ResourceAsyncImageTask, Action: ActionRead}
	AsyncImageTaskManage  = Permission{Resource: ResourceAsyncImageTask, Action: ActionManage}
	ImageModerationRead   = Permission{Resource: ResourceImageModeration, Action: ActionRead}
	ImageModerationManage = Permission{Resource: ResourceImageModeration, Action: ActionManage}
	ImageConfigRead       = Permission{Resource: ResourceImageConfig, Action: ActionRead}
	ImageConfigManage     = Permission{Resource: ResourceImageConfig, Action: ActionManage}
)

func init() {
	for _, resource := range []ResourceDefinition{
		{Resource: ResourceAsyncImageTask, LabelKey: "Async Image Tasks", Actions: []ActionDefinition{{Action: ActionRead, LabelKey: "View image tasks", DescriptionKey: "View all users' image tasks and execution history", DefaultRoles: []string{BuiltInRoleAdmin}}, {Action: ActionManage, LabelKey: "Manage image tasks", DescriptionKey: "Recover image post-processing or terminate tasks", DefaultRoles: []string{BuiltInRoleAdmin}}}},
		{Resource: ResourceImageModeration, LabelKey: "Image Moderation", Actions: []ActionDefinition{{Action: ActionRead, LabelKey: "View image moderation", DescriptionKey: "View submissions, reports and stored image assets", DefaultRoles: []string{BuiltInRoleAdmin}}, {Action: ActionManage, LabelKey: "Review image publications", DescriptionKey: "Review submissions, resolve reports and clean unreferenced images", DefaultRoles: []string{BuiltInRoleAdmin}}}},
		{Resource: ResourceImageConfig, LabelKey: "Image Settings", Actions: []ActionDefinition{{Action: ActionRead, LabelKey: "View image settings", DescriptionKey: "View image runtime, storage and channel pool settings"}, {Action: ActionManage, LabelKey: "Manage image storage and policies", DescriptionKey: "Configure image runtime, storage, platform policies and channel pools"}}},
	} {
		RegisterResource(resource)
	}
}
