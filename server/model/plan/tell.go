package plan

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"plandex-server/db"
	"plandex-server/model"
	"plandex-server/model/lib"
	"plandex-server/model/prompts"
	"plandex-server/types"

	"github.com/plandex/plandex/shared"
	"github.com/sashabaranov/go-openai"
)

// Proposal function to create a new proposal
func Tell(plan *db.Plan, currentUserId, currentOrgId string, req *shared.TellPlanRequest) error {
	// goEnv := os.Getenv("GOENV") // Fetch the GO_ENV environment variable

	// log.Println("GOENV: " + goEnv)
	// if goEnv == "test" {
	// 	streamLoremIpsum(onStream)
	// 	return nil
	// }

	planId := plan.Id

	active := Active.Get(planId)
	if active != nil {
		return fmt.Errorf("plan %s already has an active stream", planId)
	}

	active = CreateActivePlan(planId, req.Prompt)

	go execTellPlan(plan, currentUserId, currentOrgId, req, active)

	return nil
}

func execTellPlan(plan *db.Plan, currentUserId, currentOrgId string, req *shared.TellPlanRequest, active *types.ActivePlan) {
	planId := plan.Id
	err := db.SetPlanStatus(planId, shared.PlanStatusReplying, "")
	if err != nil {
		log.Printf("Error setting plan %s status to replying: %v\n", planId, err)
		active.StreamDoneCh <- fmt.Errorf("error setting plan status to replying: %v", err)
		return
	}

	errCh := make(chan error)
	var modelContext []*db.Context
	var convo []*db.ConvoMessage
	var summaries []*db.ConvoSummary

	// get name for plan and rename it's a draft
	go func() {
		if plan.Name == "draft" {
			name, err := model.GenPlanName(req.Prompt)

			if err != nil {
				log.Printf("Error generating plan name: %v\n", err)
				errCh <- fmt.Errorf("error generating plan name: %v", err)
				return
			}

			err = db.RenamePlan(planId, name)

			if err != nil {
				log.Printf("Error renaming plan: %v\n", err)
				errCh <- fmt.Errorf("error renaming plan: %v", err)
				return
			}

			err = db.IncNumNonDraftPlans(currentUserId)

			if err != nil {
				log.Printf("Error incrementing num non draft plans: %v\n", err)
				errCh <- fmt.Errorf("error incrementing num non draft plans: %v", err)
				return
			}
		}

		errCh <- nil
	}()

	go func() {
		res, err := db.GetPlanContexts(currentOrgId, planId, true)
		if err != nil {
			log.Printf("Error getting plan modelContext: %v\n", err)
			errCh <- fmt.Errorf("error getting plan modelContext: %v", err)
			return
		}
		modelContext = res
		errCh <- nil
	}()

	go func() {
		res, err := db.GetPlanConvo(currentOrgId, planId)
		if err != nil {
			log.Printf("Error getting plan convo: %v\n", err)
			errCh <- fmt.Errorf("error getting plan convo: %v", err)
			return
		}
		convo = res

		promptTokens, err := shared.GetNumTokens(req.Prompt)
		if err != nil {
			log.Printf("Error getting prompt num tokens: %v\n", err)
			errCh <- fmt.Errorf("error getting prompt num tokens: %v", err)
			return
		}

		userMsg := db.ConvoMessage{
			OrgId:   currentOrgId,
			PlanId:  planId,
			UserId:  currentUserId,
			Role:    openai.ChatMessageRoleUser,
			Tokens:  promptTokens,
			Num:     len(convo) + 1,
			Message: req.Prompt,
		}

		_, err = db.StoreConvoMessage(&userMsg, true)

		if err != nil {
			log.Printf("Error storing user message: %v\n", err)
			errCh <- fmt.Errorf("error storing user message: %v", err)
			return
		}

		errCh <- nil
	}()

	go func() {
		res, err := db.GetPlanSummaries(planId)
		if err != nil {
			log.Printf("Error getting plan summaries: %v\n", err)
			errCh <- fmt.Errorf("error getting plan summaries: %v", err)
			return
		}
		summaries = res

		errCh <- nil
	}()

	for i := 0; i < 4; i++ {
		err := <-errCh
		if err != nil {
			active.StreamDoneCh <- fmt.Errorf("error getting plan, context, convo, or summaries: %v", err)
			return
		}
	}

	Active.Update(planId, func(ap *types.ActivePlan) {
		ap.Contexts = modelContext
		ap.PromptMessageNum = len(convo) + 1

		for _, context := range modelContext {
			if context.FilePath != "" {
				ap.ContextsByPath[context.FilePath] = context
			}
		}
	})

	modelContextText, modelContextTokens, err := lib.FormatModelContext(modelContext)
	if err != nil {
		err = fmt.Errorf("error formatting model modelContext: %v", err)
		log.Println(err)
		active.StreamDoneCh <- err
		return
	}

	systemMessageText := prompts.SysCreate + modelContextText
	systemMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemMessageText,
	}

	messages := []openai.ChatCompletionMessage{
		systemMessage,
	}

	numPromptTokens, err := shared.GetNumTokens(req.Prompt)
	if err != nil {
		err = fmt.Errorf("error getting number of tokens in prompt: %v", err)
		log.Println(err)
		active.StreamDoneCh <- err
		return
	}

	promptTokens := prompts.PromptWrapperTokens + numPromptTokens
	totalTokens := prompts.CreateSysMsgNumTokens + modelContextTokens + promptTokens

	// print out breakdown of token usage
	log.Printf("System message tokens: %d\n", prompts.CreateSysMsgNumTokens)
	log.Printf("Context tokens: %d\n", modelContextTokens)
	log.Printf("Prompt tokens: %d\n", promptTokens)
	log.Printf("Total tokens before convo: %d\n", totalTokens)

	if totalTokens > shared.MaxTokens {
		// token limit already exceeded before adding conversation
		err := fmt.Errorf("token limit exceeded before adding conversation")
		log.Printf("Error: %v\n", err)
		active.StreamDoneCh <- err
		return
	}

	conversationTokens := 0
	tokensUpToTimestamp := make(map[time.Time]int)
	for _, convoMessage := range convo {
		conversationTokens += convoMessage.Tokens
		tokensUpToTimestamp[convoMessage.CreatedAt] = conversationTokens
		// log.Printf("Timestamp: %s | Tokens: %d | Total: %d | conversationTokens\n", convoMessage.Timestamp, convoMessage.Tokens, conversationTokens)
	}

	log.Printf("Conversation tokens: %d\n", conversationTokens)

	var summary *db.ConvoSummary
	var summarizedToMessageId string
	if (totalTokens+conversationTokens) > shared.MaxTokens ||
		conversationTokens > shared.MaxConvoTokens {
		log.Println("Token limit exceeded. Attempting to reduce via conversation summary.")

		// token limit exceeded after adding conversation
		// get summary for as much as the conversation as necessary to stay under the token limit
		for _, s := range summaries {
			tokens, ok := tokensUpToTimestamp[s.LatestConvoMessageCreatedAt]

			log.Printf("Last message id: %s\n", s.LatestConvoMessageCreatedAt)

			if !ok {
				err := fmt.Errorf("conversation summary timestamp not found in conversation")
				log.Printf("Error: %v\n", err)
				active.StreamDoneCh <- err
				return
			}

			updatedConversationTokens := (conversationTokens - tokens) + s.Tokens
			savedTokens := conversationTokens - updatedConversationTokens

			log.Printf("Conversation summary tokens: %d\n", tokens)
			log.Printf("Updated conversation tokens: %d\n", updatedConversationTokens)
			log.Printf("Saved tokens: %d\n", savedTokens)

			if updatedConversationTokens <= shared.MaxConvoTokens &&
				(totalTokens+updatedConversationTokens) <= shared.MaxTokens {
				log.Printf("Summarizing up to %s | saving %d tokens\n", s.LatestConvoMessageCreatedAt.Format(time.RFC3339), savedTokens)
				summary = s
				break
			}
		}

		if summary == nil {
			err := errors.New("couldn't get under token limit with conversation summary")
			log.Printf("Error: %v\n", err)
			active.StreamDoneCh <- err
			return
		}
	}

	if summary == nil {
		for _, convoMessage := range convo {
			messages = append(messages, openai.ChatCompletionMessage{
				Role:    convoMessage.Role,
				Content: convoMessage.Message,
			})
		}
	} else {
		if (totalTokens + summary.Tokens) > shared.MaxTokens {
			err := fmt.Errorf("token limit still exceeded after summarizing conversation")
			active.StreamDoneCh <- err
			return
		}
		summarizedToMessageId = summary.LatestConvoMessageId
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: summary.Summary,
		})

		// add messages after the last message in the summary
		for _, convoMessage := range convo {
			if convoMessage.CreatedAt.After(summary.LatestConvoMessageCreatedAt) {
				messages = append(messages, openai.ChatCompletionMessage{
					Role:    convoMessage.Role,
					Content: convoMessage.Message,
				})
			}
		}
	}

	promptMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf(prompts.PromptWrapperFormatStr, req.Prompt),
	}

	messages = append(messages, promptMessage)

	// log.Println("\n\nMessages:")
	// for _, message := range messages {
	// 	log.Printf("%s: %s\n", message.Role, message.Content)
	// }

	replyInfo := types.NewReplyInfo()

	modelReq := openai.ChatCompletionRequest{
		Model:       model.PlannerModel,
		Messages:    messages,
		Stream:      true,
		Temperature: 0.6,
		TopP:        0.7,
	}

	stream, err := model.Client.CreateChatCompletionStream(active.Ctx, modelReq)
	if err != nil {
		log.Printf("Error creating proposal GPT4 stream: %v\n", err)
		log.Println(err)

		errStr := err.Error()
		if strings.Contains(errStr, "status code: 400") &&
			strings.Contains(errStr, "reduce the length of the messages") {
			log.Println("Token limit exceeded")
		}

		active.StreamDoneCh <- err
		return
	}

	storeAssistantReply := func() (*db.ConvoMessage, []string, string, error) {
		files, _, _, replyNumTokens := replyInfo.FinishAndRead()

		assistantMsg := db.ConvoMessage{
			OrgId:   currentOrgId,
			PlanId:  planId,
			UserId:  currentUserId,
			Role:    openai.ChatMessageRoleAssistant,
			Tokens:  replyNumTokens,
			Num:     len(convo) + 2,
			Message: Active.Get(planId).Content,
		}

		commitMsg, err := db.StoreConvoMessage(&assistantMsg, false)

		if err != nil {
			log.Printf("Error storing assistant message: %v\n", err)
			return nil, files, "", err
		}

		Active.Update(planId, func(active *types.ActivePlan) {
			active.AssistantMessageId = assistantMsg.Id
		})

		return &assistantMsg, files, commitMsg, err
	}

	onError := func(streamErr error, storeDesc bool, convoMessageId, commitMsg string) {
		log.Printf("\nStream error: %v\n", streamErr)
		active.StreamDoneCh <- streamErr

		storedMessage := false
		storedDesc := false

		if convoMessageId == "" {
			assistantMsg, _, msg, err := storeAssistantReply()
			if err == nil {
				convoMessageId = assistantMsg.Id
				commitMsg = msg
				storedMessage = true
			} else {
				log.Printf("Error storing assistant message after stream error: %v\n", err)
			}
		}

		if storeDesc && convoMessageId != "" {
			err = db.StoreDescription(&db.ConvoMessageDescription{
				OrgId:                 currentOrgId,
				PlanId:                planId,
				SummarizedToMessageId: summarizedToMessageId,
				MadePlan:              false,
				ConvoMessageId:        convoMessageId,
				Error:                 streamErr.Error(),
			})
			if err == nil {
				storedDesc = true
			} else {
				log.Printf("Error storing description after stream error: %v\n", err)
			}
		}

		if storedMessage || storedDesc {
			err = db.GitAddAndCommit(currentOrgId, planId, commitMsg)
			if err != nil {
				log.Printf("Error committing after stream error: %v\n", err)
			}
		}
	}

	go func() {
		defer stream.Close()

		// Create a timer that will trigger if no chunk is received within the specified duration
		timer := time.NewTimer(model.OPENAI_STREAM_CHUNK_TIMEOUT)
		defer timer.Stop()

		for {
			select {
			case <-active.Ctx.Done():
				// The main modelContext was canceled (not the timer)
				return
			case <-timer.C:
				// Timer triggered because no new chunk was received in time
				log.Println("\nStream timeout due to inactivity")
				onError(fmt.Errorf("stream timeout due to inactivity"), true, "", "")
				return
			default:
				response, err := stream.Recv()

				if err == nil {
					// Successfully received a chunk, reset the timer
					if !timer.Stop() {
						<-timer.C
					}
					timer.Reset(model.OPENAI_STREAM_CHUNK_TIMEOUT)
				}

				if err != nil {
					onError(fmt.Errorf("stream error: %v", err), true, "", "")
					return
				}

				if len(response.Choices) == 0 {
					onError(fmt.Errorf("stream finished with no choices"), true, "", "")
					return
				}

				if len(response.Choices) > 1 {
					onError(fmt.Errorf("stream finished with more than one choice"), true, "", "")
					return
				}

				choice := response.Choices[0]

				if choice.FinishReason != "" {
					active.StreamCh <- shared.STREAM_DESCRIPTION_PHASE
					err := db.SetPlanStatus(planId, shared.PlanStatusDescribing, "")
					if err != nil {
						onError(fmt.Errorf("failed to set plan status to describing: %v", err), true, "", "")
						return
					}

					if len(convo) > 0 {
						// summarize in the background
						go summarizeConvo(summarizeConvoParams{
							planId:        planId,
							convo:         convo,
							summaries:     summaries,
							promptMessage: promptMessage,
							currentOrgId:  currentOrgId,
						})
					}

					assistantMsg, files, convoCommitMsg, err := storeAssistantReply()

					if err != nil {
						onError(fmt.Errorf("failed to store assistant message: %v", err), true, "", "")
						return
					}

					var description *db.ConvoMessageDescription

					if len(files) == 0 {
						description = &db.ConvoMessageDescription{
							OrgId:                 currentOrgId,
							PlanId:                planId,
							ConvoMessageId:        assistantMsg.Id,
							SummarizedToMessageId: summarizedToMessageId,
							MadePlan:              false,
						}
					} else {
						Active.Update(planId, func(ap *types.ActivePlan) {
							ap.Files = files
						})

						description, err = genPlanDescription(planId, active.Ctx)
						if err != nil {
							onError(fmt.Errorf("failed to generate plan description: %v", err), true, assistantMsg.Id, convoCommitMsg)
							return
						}

						description.OrgId = currentOrgId
						description.ConvoMessageId = assistantMsg.Id
						description.SummarizedToMessageId = summarizedToMessageId
						description.MadePlan = true
						description.Files = files
					}

					err = db.StoreDescription(description)

					if err != nil {
						onError(fmt.Errorf("failed to store description: %v", err), false, assistantMsg.Id, convoCommitMsg)
						return
					}

					err = db.GitAddAndCommit(currentOrgId, planId, convoCommitMsg)
					if err != nil {
						onError(fmt.Errorf("failed to commit: %v", err), false, assistantMsg.Id, convoCommitMsg)
						return
					}

					if len(files) == 0 {
						active.StreamCh <- shared.STREAM_FINISHED
						active.StreamDoneCh <- nil
					} else {
						active.StreamCh <- shared.STREAM_BUILD_PHASE
						Build(currentOrgId, planId)
					}
					return
				}

				delta := choice.Delta
				content := delta.Content
				Active.Update(planId, func(active *types.ActivePlan) {
					active.Content += content
					active.NumTokens++
				})

				// log.Printf("%s", content)
				active.StreamCh <- content
				replyInfo.AddToken(content, true)

			}
		}
	}()
}

type summarizeConvoParams struct {
	planId        string
	convo         []*db.ConvoMessage
	summaries     []*db.ConvoSummary
	promptMessage openai.ChatCompletionMessage
	currentOrgId  string
}

func summarizeConvo(params summarizeConvoParams) error {
	planId := params.planId
	convo := params.convo
	summaries := params.summaries
	promptMessage := params.promptMessage
	currentOrgId := params.currentOrgId

	log.Println("Generating plan summary for planId:", planId)

	var summaryMessages []openai.ChatCompletionMessage
	var latestSummary *db.ConvoSummary
	var numMessagesSummarized int = 0
	var latestMessageSummarizedAt time.Time
	var latestMessageId string
	if len(summaries) > 0 {
		latestSummary = summaries[len(summaries)-1]
		numMessagesSummarized = latestSummary.NumMessages
	}

	if latestSummary == nil {
		for _, convoMessage := range convo {
			summaryMessages = append(summaryMessages, openai.ChatCompletionMessage{
				Role:    convoMessage.Role,
				Content: convoMessage.Message,
			})
			latestMessageId = convoMessage.Id
			latestMessageSummarizedAt = convoMessage.CreatedAt
		}
	} else {
		summaryMessages = append(summaryMessages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: latestSummary.Summary,
		})
		latestMessageId = latestSummary.LatestConvoMessageId
		latestMessageSummarizedAt = latestSummary.LatestConvoMessageCreatedAt
	}

	summaryMessages = append(summaryMessages, promptMessage, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: Active.Get(planId).Content,
	})

	summary, err := model.PlanSummary(model.PlanSummaryParams{
		Conversation:                summaryMessages,
		LatestConvoMessageId:        latestMessageId,
		LatestConvoMessageCreatedAt: latestMessageSummarizedAt,
		NumMessages:                 numMessagesSummarized + 1,
		OrgId:                       currentOrgId,
		PlanId:                      planId,
	})

	if err != nil {
		log.Printf("Error generating plan summary for plan %s: %v\n", planId, err)
		return err
	}

	log.Println("Generated plan summary for plan", planId)

	err = db.StoreSummary(summary)

	if err != nil {
		log.Printf("Error storing plan summary for plan %s: %v\n", planId, err)
		return err
	}

	return nil
}