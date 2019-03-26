package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/bndr/gojenkins"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
	"gopkg.in/robfig/cron.v3"

	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/plugin"
)

const (
	JENKINS_LASTBUILD_KEY = "_JenkinsLastBuild"
	BLAME_USERNAME        = "Burnning Jenkins"
	BLAME_ICON_URL        = "https://raw.githubusercontent.com/jenkins-infra/jenkins.io/master/content/images/logos/fire/fire.png"
)

type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	BotUserID string
	TeamID    string
	ChannelID string
}

func (p *Plugin) OnActivate() error {
	split := strings.Split(p.configuration.TeamChannel, ",")
	teamSplit := split[0]
	channelSplit := split[1]

	team, err := p.API.GetTeamByName(teamSplit)
	if err != nil {
		return err
	}
	p.TeamID = team.Id

	user, err := p.API.GetUserByUsername(p.configuration.UserName)
	if err != nil {
		p.API.LogError(err.Error())
		return fmt.Errorf("Unable to find user with configured username: %v", p.configuration.UserName)
	}
	p.BotUserID = user.Id

	channel, err := p.API.GetChannelByName(team.Id, channelSplit, false)
	if err != nil && err.StatusCode == http.StatusNotFound {
		channelToCreate := &model.Channel{
			Name:        channelSplit,
			DisplayName: channelSplit,
			Type:        model.CHANNEL_OPEN,
			TeamId:      p.TeamID,
			CreatorId:   p.BotUserID,
		}

		newChannel, errChannel := p.API.CreateChannel(channelToCreate)
		if err != nil {
			p.API.LogError(err.Error())
			return errChannel
		}
		p.ChannelID = newChannel.Id
	} else if err != nil {
		return err
	} else {
		p.ChannelID = channel.Id
	}

	p.startCron()

	return nil
}

func (p *Plugin) startCron() {
	c := cron.New()
	c.AddFunc("@every 20m", p.checkJenkinsJob)
	go c.Start()
}

func (p *Plugin) checkJenkinsJob() {
	jenkins := gojenkins.CreateJenkins(nil, p.configuration.JenkinsServer,
		p.configuration.JenkinsUserName, p.configuration.JenkinsUserToken)
	_, errJenkins := jenkins.Init()
	if errJenkins != nil {
		p.API.LogError("Error creating the jenkins client", "Err", errJenkins.Error())
		return
	}

	build, errJenkins := jenkins.GetJob(p.configuration.JenkinsJob)
	if errJenkins != nil {
		p.API.LogError("Job does not exis", "Jenkins Job", p.configuration.JenkinsJob, "Err", errJenkins.Error())
		return
	}

	lastBuildToChecked, _ := p.API.KVGet(JENKINS_LASTBUILD_KEY)
	if lastBuildToChecked == nil {
		// first time
		buildIDs, errJenkins := build.GetAllBuildIds()
		if errJenkins != nil {
			fmt.Println(errJenkins.Error())
			os.Exit(1)
		}
		var last3Builds []bool
		for i, buildID := range buildIDs {
			if i == 0 {
				lastBuildToChecked = []byte(strconv.Itoa(int(buildID.Number)))
				p.API.KVSet(JENKINS_LASTBUILD_KEY, lastBuildToChecked)
			}
			job, _ := build.GetBuild(buildID.Number)
			last3Builds = append(last3Builds, job.IsGood())
			if i == 2 {
				break
			}
		}
		blame := true
		for _, isGood := range last3Builds {
			if isGood {
				blame = false
			}
		}

		if blame {
			post := &model.Post{
				UserId:    p.BotUserID,
				ChannelId: p.ChannelID,
				Message:   fmt.Sprintf("last build 3 build failed, please check"),
			}

			_, err := p.API.CreatePost(post)
			if err != nil {
				// return err
				return
			}
		}

	} else {
		lastBuild, errJenkins := build.GetLastBuild()
		if errJenkins != nil {
			p.API.LogError("Error getting the last build on Jenkins", "Err", errJenkins.Error())
			return
		}
		if string(lastBuildToChecked) == strconv.Itoa(int(lastBuild.GetBuildNumber())) || lastBuild.IsRunning() {
			return
		}
		buildIDs, errJenkins := build.GetAllBuildIds()
		if errJenkins != nil {
			p.API.LogError("Error getting all build ids on Jenkins", "Err", errJenkins.Error())
			return
		}

		var last3Builds []bool
		for i, buildID := range buildIDs {
			job, _ := build.GetBuild(buildID.Number)
			if i == 0 {
				if job.IsRunning() {
					p.API.LogInfo("Jenkins Job running, will check later...")
					return
				}
				lastBuildChecked := []byte(strconv.Itoa(int(buildID.Number)))
				p.API.KVSet(JENKINS_LASTBUILD_KEY, lastBuildChecked)
			}
			last3Builds = append(last3Builds, job.IsGood())
			if i == 2 {
				break
			}
		}
		blame := true
		for _, isGood := range last3Builds {
			if isGood {
				blame = false
			}
		}

		var msgTestResults []string
		testResult, errResults := lastBuild.GetResultSet()
		if errResults != nil {
			p.API.LogError("Error getting the tests results.", "Err", errResults.Error())
		} else {
			if testResult.FailCount != 0 {
				msgTestResults = append(msgTestResults, fmt.Sprintf("Will show the test result from the last build [#%d](%s)", lastBuild.GetBuildNumber(), lastBuild.GetUrl()))
				msgTestResults = append(msgTestResults, fmt.Sprintf("**FailCount:** `%d` **PassCount:** `%d`", testResult.FailCount, testResult.PassCount))
				for _, suite := range testResult.Suites {
					for _, suiteCase := range suite.Cases {
						if suiteCase.Status == "FAILED" || suiteCase.Status == "REGRESSION" || suiteCase.Status == "FAIL" {
							msgTestResults = append(msgTestResults, fmt.Sprintf("**Test Name:** `%s` **Status:** `%s` ", suiteCase.Name, suiteCase.Status))
						}
					}
				}
			}
		}

		if blame {
			post := &model.Post{
				UserId:    p.BotUserID,
				ChannelId: p.ChannelID,
				Props: model.StringInterface{
					"override_username": BLAME_USERNAME,
					"override_icon_url": BLAME_ICON_URL,
					"from_webhook":      "true",
				},
			}
			commitsMsg, err := p.GetLast3Commiters()
			if err != nil {
				post.Message = "The last three builds on Jenkins failed, please check the commits. I was unable to get the information for you. Sorry."
			}
			if len(msgTestResults) > 0 {
				testResultMsg := strings.Join(msgTestResults, "\n")
				post.Message = testResultMsg + "\n\n" + commitsMsg
			} else {
				post.Message = commitsMsg
			}
			_, errPost := p.API.CreatePost(post)
			if errPost != nil {
				p.API.LogError("Error creating the post", "Err", errPost.Error())
				return
			}
		}
	}
	return
}

func (p *Plugin) GetLast3Commiters() (string, error) {
	var client *github.Client
	var ctx = context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.configuration.GitHubToken})
	tc := oauth2.NewClient(ctx, ts)
	client = github.NewClient(tc)

	var msg []string
	msg = append(msg, fmt.Sprintf("**Last 3 Commiters for the following Repositories:**"))
	repositories := strings.Split(p.configuration.GitHubRepositories, ",")
	for _, repo := range repositories {
		repoSplit := strings.Split(repo, "/")
		masterCommits, _, err := client.Repositories.ListCommits(ctx, repoSplit[0], repoSplit[1], nil)
		if err != nil {
			return "", fmt.Errorf("Error when getting the list of commits. Please check if that exists, err=%v", err)
		}
		msg = append(msg, fmt.Sprintf("**Repository**: `%s`", repo))

		for i, commit := range masterCommits {
			msg = append(msg, fmt.Sprintf("1. @%s - `SHA`:[%s](%s)", commit.Author.GetLogin(), *commit.SHA, commit.GetHTMLURL()))
			if i == 2 {
				break
			}
		}

	}
	return strings.Join(msg, "\n"), nil
}
