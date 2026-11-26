# Schedule a local test-failure summary

The template reads one selected UTF-8 report on the worker labeled `summaries` and saves a short summary in run history. The orchestrator sends the rendered prompt to your named Ollama or Herd endpoint. For a two-Mac pilot, put the report on the second worker and host the model on the first Mac.

1. Using the runtime's own tools, load a local model such as `qwen2.5:3b`. In **AI connections**, add an Ollama connection with the runtime's base URL and the exact allowed model ID. A loopback endpoint can use HTTP. For a runtime on another Mac, use an HTTPS endpoint whose certificate the orchestrator trusts. A local TLS reverse proxy can serve Ollama's HTTP listener. Test the connection. Use a trusted runtime or router configured for local inference.
2. On the designated worker, run the following from this example directory. The fixture represents a previously collected CI report. Your collection process can replace it before each scheduled run.

   ```sh
   sudo install -o root -g _sluiceworker -m 0640 failures.txt /Library/Sluicework/inputs/failures.txt
   ```

3. Add the `summaries` machine label in **Machines**. In **Jobs**, approve the AI task using the settings in `template.json`. Select the named local connection and its model. Declare the required `report` input and approve the exact worker file path from the template. API clients can send the template to `POST /api/v1/jobs` after replacing its connection ID and model.
4. Run the task and select `/Library/Sluicework/inputs/failures.txt` for `report`. In **Runs**, confirm the designated worker and inspect the summary. With the fixture, look for both stale cache observations and the changed Unicode filename.
5. In **Jobs → Schedules**, create a weekday schedule with `0 9 * * 1-5`, your IANA time zone, and the same report selection. Confirm the next run. After it runs, inspect the occurrence and its output.
6. To exercise failure handling, stop the model endpoint and run the template again. The attempt should fail and the connection should show the latest provider error. Restart the runtime, test the connection, and retry the run to create a separate attempt.
