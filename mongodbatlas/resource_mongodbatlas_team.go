package mongodbatlas

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

	matlas "go.mongodb.org/atlas/mongodbatlas"
)

const (
	errorTeamCreate   = "error creating Team information: %s"
	errorTeamAddUsers = "error adding users to the Team information: %s"
	errorTeamRead     = "error getting Team information: %s"
	errorTeamUpdate   = "error updating Team information: %s"
	errorTeamDelete   = "error deleting Team (%s): %s"
	errorTeamSetting  = "error setting `%s` for Team (%s): %s"
)

func resourceMongoDBAtlasTeam() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceMongoDBAtlasTeamCreate,
		ReadContext:   resourceMongoDBAtlasTeamRead,
		UpdateContext: resourceMongoDBAtlasTeamUpdate,
		DeleteContext: resourceMongoDBAtlasTeamDelete,
		Importer: &schema.ResourceImporter{
			StateContext: resourceMongoDBAtlasTeamImportState,
		},
		Schema: map[string]*schema.Schema{
			"org_id": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"team_id": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"name": {
				Type:     schema.TypeString,
				Required: true,
			},
			"usernames": {
				Type:     schema.TypeSet,
				Required: true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
		},
	}
}

func resourceMongoDBAtlasTeamCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*MongoDBClient).Atlas
	orgID := d.Get("org_id").(string)

	// Creating the team
	teamsResp, _, err := conn.Teams.Create(ctx, orgID,
		&matlas.Team{
			Name:      d.Get("name").(string),
			Usernames: expandStringListFromSetSchema(d.Get("usernames").(*schema.Set)),
		})
	if err != nil {
		return diag.FromErr(fmt.Errorf(errorTeamCreate, err))
	}

	d.SetId(encodeStateID(map[string]string{
		"org_id": orgID,
		"id":     teamsResp.ID,
	}))

	return resourceMongoDBAtlasTeamRead(ctx, d, meta)
}

func resourceMongoDBAtlasTeamRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*MongoDBClient).Atlas

	ids := decodeStateID(d.Id())
	orgID := ids["org_id"]
	teamID := ids["id"]

	team, resp, err := conn.Teams.Get(context.Background(), orgID, teamID)

	if err != nil {
		// new resource missing
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			d.SetId("")
			return nil
		}
		return diag.FromErr(fmt.Errorf(errorTeamRead, err))
	}

	if err = d.Set("name", team.Name); err != nil {
		return diag.FromErr(fmt.Errorf(errorTeamSetting, "name", teamID, err))
	}

	if err = d.Set("team_id", team.ID); err != nil {
		return diag.FromErr(fmt.Errorf(errorTeamSetting, "team_id", teamID, err))
	}

	// Set Usernames
	users, _, err := conn.Teams.GetTeamUsersAssigned(ctx, orgID, teamID)
	if err != nil {
		return diag.FromErr(fmt.Errorf(errorTeamRead, err))
	}

	usernames := []string{}
	for i := range users {
		usernames = append(usernames, users[i].Username)
	}

	if err := d.Set("usernames", usernames); err != nil {
		return diag.FromErr(fmt.Errorf(errorTeamSetting, "usernames", teamID, err))
	}

	return nil
}

func resourceMongoDBAtlasTeamUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*MongoDBClient).Atlas

	ids := decodeStateID(d.Id())
	orgID := ids["org_id"]
	teamID := ids["id"]

	if d.HasChange("name") {
		_, _, err := conn.Teams.Rename(ctx, orgID, teamID, d.Get("name").(string))
		if err != nil {
			return diag.FromErr(fmt.Errorf(errorTeamUpdate, err))
		}
	}

	if d.HasChange("usernames") {
		// First, we need to remove the current users of the team and later add the new users
		// Get the current team's users
		users, _, err := conn.Teams.GetTeamUsersAssigned(ctx, orgID, teamID)

		if err != nil {
			return diag.FromErr(fmt.Errorf(errorTeamRead, err))
		}

		// Removing each user - Let's not modify the state before making sure we can continue

		// existig users
		index := make(map[string]matlas.AtlasUser)
		for i := range users {
			index[users[i].Username] = users[i]
		}

		cleanUsers := func() error {
			for i := range users {
				_, err := conn.Teams.RemoveUserToTeam(ctx, orgID, teamID, users[i].ID)
				if err != nil {
					return fmt.Errorf("error deleting Atlas User (%s) information: %s", teamID, err)
				}
			}
			return nil
		}

		// existing users

		// Verify if the gave users exists
		var newUsers []string

		for _, username := range d.Get("usernames").(*schema.Set).List() {
			user, _, err := conn.AtlasUsers.GetByName(ctx, username.(string))

			updatedUserData := user

			if err != nil {
				// this must be handle as a soft error
				if !strings.Contains(err.Error(), "401") {
					// In this case is a hard error doing a rollback from the initial operation
					return diag.FromErr(fmt.Errorf("error getting Atlas User (%s) information: %s", username, err))
				}

				log.Printf("[WARN] error fetching information user for (%s): %s\n", username, err)
				if user == nil {
					log.Printf("[WARN] there is no runtime information to fetch, checking in the existing users")

					cached, ok := index[username.(string)]

					if !ok {
						log.Printf("[WARN] no information in cached for (%s)", username)
						return diag.FromErr(fmt.Errorf("error getting Atlas User (%s) information: %s", username, err))
					}
					updatedUserData = &cached
				}
			}
			// if the user exists, we will storage its teamID
			newUsers = append(newUsers, updatedUserData.ID)
		}

		// Update the users, remove the old ones, add the new ones
		err = cleanUsers()
		if err != nil {
			return diag.FromErr(err)
		}

		_, _, err = conn.Teams.AddUsersToTeam(ctx, orgID, teamID, newUsers)
		if err != nil {
			return diag.FromErr(fmt.Errorf(errorTeamAddUsers, err))
		}
	}

	return resourceMongoDBAtlasTeamRead(ctx, d, meta)
}

func resourceMongoDBAtlasTeamDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*MongoDBClient).Atlas
	ids := decodeStateID(d.Id())
	orgID := ids["org_id"]
	id := ids["id"]

	err := retry.RetryContext(ctx, 1*time.Hour, func() *retry.RetryError {
		_, err := conn.Teams.RemoveTeamFromOrganization(ctx, orgID, id)
		if err != nil {
			var target *matlas.ErrorResponse
			if errors.As(err, &target) && target.ErrorCode == "CANNOT_DELETE_TEAM_ASSIGNED_TO_PROJECT" {
				projectID, err := getProjectIDByTeamID(ctx, conn, id)
				if err != nil {
					return retry.NonRetryableError(err)
				}

				_, err = conn.Teams.RemoveTeamFromProject(ctx, projectID, id)
				if err != nil {
					return retry.NonRetryableError(fmt.Errorf(errorTeamDelete, id, err))
				}
				return retry.RetryableError(fmt.Errorf("will retry again"))
			}
		}
		return nil
	})
	if err != nil {
		return diag.FromErr(fmt.Errorf(errorTeamDelete, id, err))
	}

	return nil
}

func resourceMongoDBAtlasTeamImportState(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	conn := meta.(*MongoDBClient).Atlas

	parts := strings.SplitN(d.Id(), "-", 2)
	if len(parts) != 2 {
		return nil, errors.New("import format error: to import a team, use the format {group_id}-{team_id}")
	}

	orgID := parts[0]
	teamID := parts[1]

	u, _, err := conn.Teams.Get(ctx, orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("couldn't import team (%s) in organization(%s), error: %s", teamID, orgID, err)
	}

	if err := d.Set("org_id", orgID); err != nil {
		log.Printf("[WARN] Error setting org_id for (%s): %s", teamID, err)
	}

	if err := d.Set("team_id", teamID); err != nil {
		log.Printf("[WARN] Error setting team_id for (%s): %s", teamID, err)
	}

	d.SetId(encodeStateID(map[string]string{
		"org_id": orgID,
		"id":     u.ID,
	}))

	return []*schema.ResourceData{d}, nil
}

func expandStringListFromSetSchema(list *schema.Set) []string {
	res := make([]string, list.Len())
	for i, v := range list.List() {
		res[i] = v.(string)
	}

	return res
}

func getProjectIDByTeamID(ctx context.Context, conn *matlas.Client, teamID string) (string, error) {
	options := &matlas.ListOptions{}
	projects, _, err := conn.Projects.GetAllProjects(ctx, options)
	if err != nil {
		return "", fmt.Errorf("error getting projects information: %s", err)
	}

	for _, project := range projects.Results {
		teams, _, err := conn.Projects.GetProjectTeamsAssigned(ctx, project.ID)
		if err != nil {
			return "", fmt.Errorf("error getting teams from project information: %s", err)
		}

		for _, team := range teams.Results {
			if team.TeamID == teamID {
				return project.ID, nil
			}
		}
	}

	return "", nil
}
