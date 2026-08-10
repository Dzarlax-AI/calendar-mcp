package calendar

type OperationCapabilities struct {
	List      bool `json:"list"`
	Get       bool `json:"get"`
	Search    bool `json:"search"`
	Create    bool `json:"create"`
	Update    bool `json:"update"`
	Delete    bool `json:"delete"`
	Instances bool `json:"instances"`
	Respond   bool `json:"respond"`
	Move      bool `json:"move"`
	Import    bool `json:"import"`
}

type FieldCapabilities struct {
	Recurrence        bool `json:"recurrence"`
	Reminders         bool `json:"reminders"`
	Attachments       bool `json:"attachments"`
	Conferencing      bool `json:"conferencing"`
	Colors            bool `json:"colors"`
	GuestPermissions  bool `json:"guest_permissions"`
	ExtendedProps     bool `json:"extended_properties"`
	SpecialEventTypes bool `json:"special_event_types"`
	OptimisticLocking bool `json:"optimistic_locking"`
}

type CalendarCapabilities struct {
	Provider             string                `json:"provider"`
	CalendarID           string                `json:"calendar_id"`
	ReadOnly             bool                  `json:"read_only"`
	Operations           OperationCapabilities `json:"operations"`
	Fields               FieldCapabilities     `json:"fields"`
	MutationScopes       []MutationScope       `json:"mutation_scopes,omitempty"`
	NotificationPolicies []NotificationPolicy  `json:"notification_policies,omitempty"`
	EventTypes           []string              `json:"event_types,omitempty"`
	Reasons              map[string]string     `json:"reasons,omitempty"`
}

func (c CalendarCapabilities) SupportsNotifications(policy NotificationPolicy) bool {
	for _, supported := range c.NotificationPolicies {
		if supported == policy {
			return true
		}
	}
	return false
}

func (c CalendarCapabilities) SupportsScope(scope MutationScope) bool {
	for _, supported := range c.MutationScopes {
		if supported == scope {
			return true
		}
	}
	return false
}
