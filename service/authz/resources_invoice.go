package authz

const ResourceInvoice = "invoice"

var (
	InvoiceRead   = Permission{Resource: ResourceInvoice, Action: ActionRead}
	InvoiceManage = Permission{Resource: ResourceInvoice, Action: ActionManage}
)

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceInvoice,
		LabelKey: "Invoices",
		Actions: []ActionDefinition{
			{
				Action:         ActionRead,
				LabelKey:       "View invoice requests",
				DescriptionKey: "View users' invoice requests and invoice history",
				DefaultRoles:   []string{BuiltInRoleAdmin},
			},
			{
				Action:         ActionManage,
				LabelKey:       "Manage invoice requests",
				DescriptionKey: "Complete or reject invoice requests and record historical invoices",
				DefaultRoles:   []string{BuiltInRoleAdmin},
			},
		},
	})
}
