// Copyright 2023 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func BenchmarkCoreRequestReply(b *testing.B) {
	const (
		subject = "test-subject"
	)

	messageSizes := []int64{
		1024,   // 1kb
		4096,   // 4kb
		40960,  // 40kb
		409600, // 400kb
	}

	for _, messageSize := range messageSizes {
		b.Run(fmt.Sprintf("msgSz=%db", messageSize), func(b *testing.B) {

			// Start server
			serverOpts := DefaultOptions()
			server := RunServer(serverOpts)
			defer server.Shutdown()

			clientUrl := server.ClientURL()

			// Create "echo" subscriber
			ncSub, err := nats.Connect(clientUrl)
			if err != nil {
				b.Fatal(err)
			}
			defer ncSub.Close()
			sub, err := ncSub.Subscribe(subject, func(msg *nats.Msg) {
				// Responder echoes the request payload as-is
				msg.Respond(msg.Data)
			})
			defer sub.Unsubscribe()
			if err != nil {
				b.Fatal(err)
			}

			// Create publisher
			ncPub, err := nats.Connect(clientUrl)
			if err != nil {
				b.Fatal(err)
			}
			defer ncPub.Close()

			var errors = 0

			// Create message (reused for all requests)
			messageData := make([]byte, messageSize)
			b.SetBytes(messageSize)
			rand.Read(messageData)

			// Benchmark
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := ncPub.Request(subject, messageData, time.Second)
				if err != nil {
					errors++
				}
			}
			b.StopTimer()

			b.ReportMetric(float64(errors), "errors")
		})
	}
}

func BenchmarkCoreTLSFanOut(b *testing.B) {
	const (
		subject            = "test-subject"
		configsBasePath    = "./configs/tls"
		maxPendingMessages = 25
		maxPendingBytes    = 15 * 1024 * 1024 // 15MiB
	)

	keyTypeCases := []string{
		"none",
		"ed25519",
		"rsa-1024",
		"rsa-2048",
		"rsa-4096",
	}
	messageSizeCases := []int64{
		512 * 1024, // 512Kib
	}
	numSubsCases := []int{
		5,
	}

	// Custom error handler that ignores ErrSlowConsumer.
	// Lots of them are expected in this benchmark which indiscriminately publishes at a rate higher
	// than what the server can relay to subscribers.
	ignoreSlowConsumerErrorHandler := func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if errors.Is(err, nats.ErrSlowConsumer) {
			// Swallow this error
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", err)
		}
	}

	for _, keyType := range keyTypeCases {

		b.Run(
			fmt.Sprintf("keyType=%s", keyType),
			func(b *testing.B) {

				for _, messageSize := range messageSizeCases {
					b.Run(
						fmt.Sprintf("msgSz=%db", messageSize),
						func(b *testing.B) {

							for _, numSubs := range numSubsCases {
								b.Run(
									fmt.Sprintf("subs=%d", numSubs),
									func(b *testing.B) {
										// Start server
										configPath := fmt.Sprintf("%s/tls-%s.conf", configsBasePath, keyType)
										server, _ := RunServerWithConfig(configPath)
										defer server.Shutdown()

										opts := []nats.Option{
											nats.MaxReconnects(-1),
											nats.ReconnectWait(0),
											nats.ErrorHandler(ignoreSlowConsumerErrorHandler),
										}

										if keyType != "none" {
											opts = append(opts, nats.Secure(&tls.Config{
												InsecureSkipVerify: true,
											}))
										}

										clientUrl := server.ClientURL()

										// Count of messages received for by each subscriber
										counters := make([]int, numSubs)

										// Wait group for subscribers to signal they received b.N messages
										var wg sync.WaitGroup
										wg.Add(numSubs)

										// Create subscribers
										for i := 0; i < numSubs; i++ {
											subIndex := i
											ncSub, err := nats.Connect(clientUrl, opts...)
											if err != nil {
												b.Fatal(err)
											}
											defer ncSub.Close()
											sub, err := ncSub.Subscribe(subject, func(_ *nats.Msg) {
												counters[subIndex] += 1
												if counters[subIndex] == b.N {
													wg.Done()
												}
											})
											if err != nil {
												b.Fatalf("failed to subscribe: %s", err)
											}
											err = sub.SetPendingLimits(maxPendingMessages, maxPendingBytes)
											if err != nil {
												b.Fatalf("failed to set pending limits: %s", err)
											}
											defer sub.Unsubscribe()
											if err != nil {
												b.Fatal(err)
											}
										}

										// publisher
										ncPub, err := nats.Connect(clientUrl, opts...)
										if err != nil {
											b.Fatal(err)
										}
										defer ncPub.Close()

										var errorCount = 0

										// random bytes as payload
										messageData := make([]byte, messageSize)
										rand.Read(messageData)

										quitCh := make(chan bool, 1)

										publish := func() {
											for {
												select {
												case <-quitCh:
													return
												default:
													// continue publishing
												}

												err := ncPub.Publish(subject, messageData)
												if err != nil {
													errorCount += 1
												}
											}
										}

										// Set bytes per operation
										b.SetBytes(messageSize)
										// Start the clock
										b.ResetTimer()
										// Start publishing as fast as the server allows
										go publish()
										// Wait for all subscribers to have delivered b.N messages
										wg.Wait()
										// Stop the clock
										b.StopTimer()

										// Stop publisher
										quitCh <- true

										b.ReportMetric(float64(errorCount), "errors")
									},
								)
							}
						},
					)
				}
			},
		)
	}
}

func BenchmarkCoreFanOut(b *testing.B) {
	const (
		subject            = "test-subject"
		maxPendingMessages = 25
		maxPendingBytes    = 15 * 1024 * 1024 // 15MiB
	)

	messageSizeCases := []int64{
		100,        // 100B
		1024,       // 1KiB
		10240,      // 10KiB
		512 * 1024, // 512KiB
	}
	numSubsCases := []int{
		3,
		5,
		10,
	}

	// Custom error handler that ignores ErrSlowConsumer.
	// Lots of them are expected in this benchmark which indiscriminately publishes at a rate higher
	// than what the server can relay to subscribers.
	ignoreSlowConsumerErrorHandler := func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if errors.Is(err, nats.ErrSlowConsumer) {
			// Swallow this error
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", err)
		}
	}

	for _, messageSize := range messageSizeCases {
		b.Run(
			fmt.Sprintf("msgSz=%db", messageSize),
			func(b *testing.B) {
				for _, numSubs := range numSubsCases {
					b.Run(
						fmt.Sprintf("subs=%d", numSubs),
						func(b *testing.B) {
							// Start server
							defaultOpts := DefaultOptions()
							server := RunServer(defaultOpts)
							defer server.Shutdown()

							opts := []nats.Option{
								nats.MaxReconnects(-1),
								nats.ReconnectWait(0),
								nats.ErrorHandler(ignoreSlowConsumerErrorHandler),
							}

							clientUrl := server.ClientURL()

							// Count of messages received for by each subscriber
							counters := make([]int, numSubs)

							// Wait group for subscribers to signal they received b.N messages
							var wg sync.WaitGroup
							wg.Add(numSubs)

							// Create subscribers
							for i := 0; i < numSubs; i++ {
								subIndex := i
								ncSub, err := nats.Connect(clientUrl, opts...)
								if err != nil {
									b.Fatal(err)
								}
								defer ncSub.Close()
								sub, err := ncSub.Subscribe(subject, func(_ *nats.Msg) {
									counters[subIndex] += 1
									if counters[subIndex] == b.N {
										wg.Done()
									}
								})
								if err != nil {
									b.Fatalf("failed to subscribe: %s", err)
								}
								err = sub.SetPendingLimits(maxPendingMessages, maxPendingBytes)
								if err != nil {
									b.Fatalf("failed to set pending limits: %s", err)
								}
								defer sub.Unsubscribe()
							}

							// publisher
							ncPub, err := nats.Connect(clientUrl, opts...)
							if err != nil {
								b.Fatal(err)
							}
							defer ncPub.Close()

							var errorCount = 0

							// random bytes as payload
							messageData := make([]byte, messageSize)
							rand.Read(messageData)

							quitCh := make(chan bool, 1)

							publish := func() {
								for {
									select {
									case <-quitCh:
										return
									default:
										// continue publishing
									}

									err := ncPub.Publish(subject, messageData)
									if err != nil {
										errorCount += 1
									}
								}
							}

							// Set bytes per operation
							b.SetBytes(messageSize)
							// Start the clock
							b.ResetTimer()
							// Start publishing as fast as the server allows
							go publish()
							// Wait for all subscribers to have delivered b.N messages
							wg.Wait()
							// Stop the clock
							b.StopTimer()

							// Stop publisher
							quitCh <- true

							b.ReportMetric(100*float64(errorCount)/float64(b.N), "%error")
						},
					)
				}
			},
		)
	}
}

func BenchmarkCoreFanIn(b *testing.B) {
	const (
		subjectBaseName = "test-subject"
		numPubs         = 5
	)

	type BenchPublisher struct {
		// nats connection for this publisher
		conn *nats.Conn
		// number of publishing errors encountered
		publishErrors int
		// number of messages published
		publishCounter int
		// quit channel which will terminate publishing
		quitCh chan bool
	}

	messageSizeCases := []int64{
		10,         // 10B
		512 * 1024, // 512KiB
	}
	numPubsCases := []int{
		3,
		5,
	}

	// Custom error handler that ignores ErrSlowConsumer.
	// Lots of them are expected in this benchmark which indiscriminately publishes at a rate higher
	// than what the server can relay to subscribers.
	ignoreSlowConsumerErrorHandler := func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if errors.Is(err, nats.ErrSlowConsumer) {
			// Swallow this error
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", err)
		}
	}

	for _, messageSize := range messageSizeCases {
		b.Run(
			fmt.Sprintf("msgSz=%db", messageSize),
			func(b *testing.B) {
				for _, numPubs := range numPubsCases {
					b.Run(
						fmt.Sprintf("pubs=%d", numPubs),
						func(b *testing.B) {

							opts := []nats.Option{
								nats.MaxReconnects(-1),
								nats.ReconnectWait(0),
								nats.ErrorHandler(ignoreSlowConsumerErrorHandler),
							}

							clientUrl := "nats://ec2-18-226-226-82.us-east-2.compute.amazonaws.com:4222,nats://ec2-18-224-38-254.us-east-2.compute.amazonaws.com:4222,nats://ec2-18-118-7-226.us-east-2.compute.amazonaws.com:4222"

							// start subscriber
							ncSub, err := nats.Connect(clientUrl, opts...)
							if err != nil {
								b.Fatal(err)
							}
							defer ncSub.Close()

							publishers := make([]BenchPublisher, numPubs)
							for i := range publishers {
								publishers[i].quitCh = make(chan bool, 1)
							}

							// total number of publishers that have published b.N to the subscriber successfully
							completedPublishersCount := 0

							// wait group blocks main thread until publish workload is completed, it is decremented after subscriber receives b.N messages from all publishers
							var benchCompleteWg sync.WaitGroup
							benchCompleteWg.Add(1)

							ncSub.Subscribe(fmt.Sprintf("%s.*", subjectBaseName), func(msg *nats.Msg) {
								// get the publisher id from subject
								pubIdx, err := strconv.Atoi(msg.Subject[len(subjectBaseName)+1:])
								if err != nil {
									b.Fatal(err)
								}

								if pubIdx >= len(publishers) {
									// unknown publisher id
									return
								}

								// message successfully received from publisher
								publishers[pubIdx].publishCounter += 1

								// subscriber has received a total of b.N messages from this publisher
								if publishers[pubIdx].publishCounter == b.N {
									completedPublishersCount++
									// every publisher has successfully sent b.N messages to subscriber
									if completedPublishersCount == numPubs {
										benchCompleteWg.Done()
									}
								}
							})

							// random bytes as payload
							messageData := make([]byte, messageSize)
							rand.Read(messageData)

							var publishersReadyWg sync.WaitGroup
							// waits for all publishers sub-routines and for main thread to be ready
							publishersReadyWg.Add(numPubs + 1)

							// wait group to ensure all publishers have been torn down
							var finishedPublishersWg sync.WaitGroup
							finishedPublishersWg.Add(numPubs)

							// create N publishers
							for i := range publishers {
								// create publisher connection and attach to bench publisher object
								ncPub, err := nats.Connect(clientUrl, opts...)
								if err != nil {
									b.Fatal(err)
								}
								publishers[i].conn = ncPub

								// publisher successfully initialized
								publishersReadyWg.Done()

								defer ncPub.Close()
							}

							for i := range publishers {
								go func(pubId int) {
									publisher := publishers[pubId]
									subject := fmt.Sprintf("%s.%d", subjectBaseName, pubId)

									// signal that this publisher has been torn down
									defer finishedPublishersWg.Done()

									// wait till all other publishers are ready to start workload
									publishersReadyWg.Wait()

									// publish until quitCh is closed
									for {
										select {
										case <-publisher.quitCh:
											return
										default:
											// continue publishing
										}
										err := publisher.conn.Publish(subject, messageData)
										if err != nil {
											publisher.publishErrors += 1
										}
									}
								}(i)
							}

							// set bytes per operation
							b.SetBytes(messageSize)
							// main thread is ready
							publishersReadyWg.Done()
							// wait till publishers are ready
							publishersReadyWg.Wait()

							// start the clock
							b.ResetTimer()
							// wait till termination cond reached
							benchCompleteWg.Wait()
							// stop the clock
							b.StopTimer()

							// send quit signal to all publishers
							for i := range publishers {
								publishers[i].quitCh <- true
							}
							// wait for all publishers to shutdown
							finishedPublishersWg.Wait()

							// sum errors from all publishers
							totalErrors := 0
							for _, publisher := range publishers {
								totalErrors += publisher.publishErrors
							}
							// sum total messages sent from all publishers
							totalMessages := 0
							for _, publisher := range publishers {
								totalMessages += publisher.publishCounter
							}
							errorRate := 100 * float64(totalErrors) / float64(totalMessages)

							// report error rate
							b.ReportMetric(errorRate, "%error")

						})
				}
			})
	}
}
