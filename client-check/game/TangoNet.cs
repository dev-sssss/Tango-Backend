using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Net.WebSockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;

namespace UnoGame.Online
{
    /// <summary>Where the Tango Cards server is. Loaded from Resources/TangoServer.json by the game.</summary>
    [Serializable]
    public sealed class TangoServerConfig
    {
        public string Scheme = "https";
        public string Host = "tango-backend.sssss.co.in";
        public int Port = 9443;
        public string ServerKey = "";
        public string BaseUrl => Scheme + "://" + Host + ((Scheme == "https" && Port == 443) || (Scheme == "http" && Port == 80) ? "" : ":" + Port);
        public string SocketUrl => (Scheme == "https" ? "wss" : "ws") + "://" + Host + ((Scheme == "https" && Port == 443) || (Scheme == "http" && Port == 80) ? "" : ":" + Port) + "/ws";
    }

    /// <summary>A signed-in session: the short access token and the long refresh token.</summary>
    public sealed class TangoSession
    {
        public string Token = "", RefreshToken = "", UserId = "", Username = "";
        public long ExpiresUnix, RefreshExpiresUnix;
        public bool Created;
        public bool ExpiresWithin(int seconds) => DateTimeOffset.UtcNow.ToUnixTimeSeconds() + seconds >= ExpiresUnix;
        public bool RefreshUsable => !string.IsNullOrEmpty(RefreshToken) && DateTimeOffset.UtcNow.ToUnixTimeSeconds() + 60 < RefreshExpiresUnix;

        public static TangoSession From(string token, string refresh)
        {
            var s = new TangoSession { Token = token ?? "", RefreshToken = refresh ?? "" };
            var claims = Claims(token);
            if (claims != null)
            {
                s.UserId = (string)claims["uid"] ?? "";
                s.Username = (string)claims["usn"] ?? "";
                s.ExpiresUnix = (long?)claims["exp"] ?? 0;
            }
            var refreshClaims = Claims(refresh);
            if (refreshClaims != null) s.RefreshExpiresUnix = (long?)refreshClaims["exp"] ?? 0;
            return s;
        }

        static JObject Claims(string jwt)
        {
            try
            {
                var parts = (jwt ?? "").Split('.');
                if (parts.Length < 2) return null;
                string b64 = parts[1].Replace('-', '+').Replace('_', '/');
                b64 = b64.PadRight(b64.Length + (4 - b64.Length % 4) % 4, '=');
                return JObject.Parse(Encoding.UTF8.GetString(Convert.FromBase64String(b64)));
            }
            catch (Exception) { return null; }
        }
    }

    /// <summary>The server answered an HTTP call with an error (status code and its message).</summary>
    public sealed class TangoHttpException : Exception
    {
        public readonly int Status;
        public TangoHttpException(int status, string message) : base(message) { Status = status; }
    }

    /// <summary>One realtime message from a match: its op code and JSON body.</summary>
    public struct TangoMatchMessage
    {
        public string MatchId;
        public long OpCode;
        public string Json;
    }

    /// <summary>
    /// Talks to the Tango Cards server: sign-in and RPCs over HTTPS, and the realtime socket for matches
    /// (Nakama's protocol in its JSON form). Messages from the socket are queued and read with <see cref="TryDequeue"/>
    /// on the game's main thread, so no Unity object is ever touched from a network thread.
    /// </summary>
    public sealed class TangoNet : IDisposable
    {
        public readonly TangoServerConfig Config;
        public TangoSession Session { get; private set; }
        public bool SocketOpen => socket != null && socket.State == WebSocketState.Open;
        /// <summary>Set when the socket closes on its own (network change, server restart).</summary>
        public event Action<string> SocketClosed;

        static readonly HttpClient Http = new HttpClient { Timeout = TimeSpan.FromSeconds(15) };
        readonly ConcurrentQueue<TangoMatchMessage> inbox = new ConcurrentQueue<TangoMatchMessage>();
        readonly ConcurrentDictionary<string, TaskCompletionSource<JObject>> pending = new ConcurrentDictionary<string, TaskCompletionSource<JObject>>();
        readonly SemaphoreSlim sendLock = new SemaphoreSlim(1, 1), refreshLock = new SemaphoreSlim(1, 1);
        ClientWebSocket socket;
        CancellationTokenSource socketStop;
        int nextCid;

        public TangoNet(TangoServerConfig config) { Config = config ?? new TangoServerConfig(); }

        // ------------------------------------------------------------------ sign-in
        public async Task<TangoSession> AuthenticateDevice(string deviceId)
        {
            var body = new JObject { ["id"] = deviceId };
            var res = await Send(HttpMethod.Post, "/v2/account/authenticate/device?create=true", body.ToString(Formatting.None), basic: true);
            Session = TangoSession.From((string)res["token"], (string)res["refresh_token"]);
            Session.Created = (bool?)res["created"] ?? false;
            return Session;
        }

        public void Restore(string token, string refresh) { Session = TangoSession.From(token, refresh); }

        /// <summary>Keeps the access token fresh (refreshes it when it has under two minutes left).</summary>
        public async Task EnsureSession()
        {
            if (Session == null) throw new InvalidOperationException("Not signed in.");
            if (!Session.ExpiresWithin(120)) return;
            await refreshLock.WaitAsync();
            try
            {
                if (!Session.ExpiresWithin(120)) return;
                if (!Session.RefreshUsable) throw new TangoHttpException(401, "Session expired.");
                var body = new JObject { ["token"] = Session.RefreshToken };
                var res = await Send(HttpMethod.Post, "/v2/account/session/refresh", body.ToString(Formatting.None), basic: true);
                Session = TangoSession.From((string)res["token"], (string)res["refresh_token"] ?? Session.RefreshToken);
            }
            finally { refreshLock.Release(); }
        }

        // ------------------------------------------------------------------ RPC
        /// <summary>Calls a server RPC with a JSON payload and returns its JSON answer.</summary>
        public async Task<JObject> Rpc(string id, object payload = null)
        {
            await EnsureSession();
            return await Send(HttpMethod.Post, "/v2/rpc/" + id + "?unwrap=true", ToJson(payload), basic: false);
        }

        async Task<JObject> Send(HttpMethod method, string path, string body, bool basic)
        {
            using (var req = new HttpRequestMessage(method, Config.BaseUrl + path))
            {
                req.Headers.Authorization = basic
                    ? new AuthenticationHeaderValue("Basic", Convert.ToBase64String(Encoding.UTF8.GetBytes(Config.ServerKey + ":")))
                    : new AuthenticationHeaderValue("Bearer", Session?.Token ?? "");
                req.Headers.Add("X-Tango-Client", "unity");
                if (body != null) req.Content = new StringContent(body, Encoding.UTF8, "application/json");
                using (var res = await Http.SendAsync(req).ConfigureAwait(false))
                {
                    string text = res.Content == null ? "" : await res.Content.ReadAsStringAsync().ConfigureAwait(false);
                    if (!res.IsSuccessStatusCode)
                    {
                        string message = text;
                        try { message = (string)JObject.Parse(text)["message"] ?? text; } catch (Exception) { }
                        throw new TangoHttpException((int)res.StatusCode, message);
                    }
                    if (string.IsNullOrWhiteSpace(text)) return new JObject();
                    var token = JToken.Parse(text);
                    return token as JObject ?? new JObject { ["value"] = token };
                }
            }
        }

        // ------------------------------------------------------------------ realtime socket
        public async Task ConnectSocket()
        {
            await EnsureSession();
            await CloseSocket();
            var ws = new ClientWebSocket();
            ws.Options.KeepAliveInterval = TimeSpan.FromSeconds(20);
            var stop = new CancellationTokenSource();
            string url = Config.SocketUrl + "?lang=en&status=true&format=json&token=" + Uri.EscapeDataString(Session.Token);
            using (var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(12)))
                await ws.ConnectAsync(new Uri(url), timeout.Token).ConfigureAwait(false);
            socket = ws; socketStop = stop;
            _ = Task.Run(() => ReceiveLoop(ws, stop.Token));
            _ = Task.Run(() => PingLoop(ws, stop.Token));
        }

        public async Task CloseSocket()
        {
            var ws = socket; var stop = socketStop;
            socket = null; socketStop = null;
            if (stop != null) stop.Cancel();
            if (ws != null)
            {
                try
                {
                    if (ws.State == WebSocketState.Open)
                        using (var t = new CancellationTokenSource(TimeSpan.FromSeconds(2)))
                            await ws.CloseAsync(WebSocketCloseStatus.NormalClosure, "", t.Token).ConfigureAwait(false);
                }
                catch (Exception) { }
                ws.Dispose();
            }
            FailPending("Connection closed.");
        }

        /// <summary>Joins a match and returns once the server has accepted the seat.</summary>
        public Task<JObject> JoinMatch(string matchId) => Request(new JObject { ["match_join"] = new JObject { ["match_id"] = matchId } });

        public Task<JObject> LeaveMatch(string matchId) => Request(new JObject { ["match_leave"] = new JObject { ["match_id"] = matchId } });

        /// <summary>Sends one intent (op code + JSON) to the match.</summary>
        public Task SendMatch(string matchId, long opCode, object payload = null)
        {
            string json = ToJson(payload);
            var env = new JObject
            {
                ["match_data_send"] = new JObject
                {
                    ["match_id"] = matchId, ["op_code"] = opCode.ToString(),
                    ["data"] = Convert.ToBase64String(Encoding.UTF8.GetBytes(json)), ["reliable"] = true
                }
            };
            return Write(env);
        }

        public bool TryDequeue(out TangoMatchMessage message) => inbox.TryDequeue(out message);

        static string ToJson(object payload)
        {
            switch (payload)
            {
                case null: return "{}";
                case string s: return s;
                case JToken t: return t.ToString(Formatting.None);
                default: return JsonConvert.SerializeObject(payload);
            }
        }

        async Task<JObject> Request(JObject env)
        {
            string cid = Interlocked.Increment(ref nextCid).ToString();
            env["cid"] = cid;
            var tcs = new TaskCompletionSource<JObject>(TaskCreationOptions.RunContinuationsAsynchronously);
            pending[cid] = tcs;
            try
            {
                await Write(env);
                var done = await Task.WhenAny(tcs.Task, Task.Delay(TimeSpan.FromSeconds(15))).ConfigureAwait(false);
                if (done != tcs.Task) throw new TimeoutException("The server did not answer in time.");
                return await tcs.Task.ConfigureAwait(false);
            }
            finally { pending.TryRemove(cid, out _); }
        }

        async Task Write(JObject env)
        {
            var ws = socket;
            if (ws == null || ws.State != WebSocketState.Open) throw new InvalidOperationException("Not connected to the game server.");
            var bytes = Encoding.UTF8.GetBytes(env.ToString(Formatting.None));
            await sendLock.WaitAsync().ConfigureAwait(false);
            try { await ws.SendAsync(new ArraySegment<byte>(bytes), WebSocketMessageType.Text, true, CancellationToken.None).ConfigureAwait(false); }
            finally { sendLock.Release(); }
        }

        async Task PingLoop(ClientWebSocket ws, CancellationToken stop)
        {
            try
            {
                while (!stop.IsCancellationRequested && ws.State == WebSocketState.Open)
                {
                    await Task.Delay(TimeSpan.FromSeconds(15), stop).ConfigureAwait(false);
                    if (socket == ws) await Write(new JObject { ["ping"] = new JObject() }).ConfigureAwait(false);
                }
            }
            catch (Exception) { }
        }

        async Task ReceiveLoop(ClientWebSocket ws, CancellationToken stop)
        {
            var buffer = new byte[16 * 1024];
            var text = new StringBuilder();
            string reason = "closed";
            try
            {
                while (!stop.IsCancellationRequested && ws.State == WebSocketState.Open)
                {
                    text.Clear();
                    WebSocketReceiveResult r;
                    do
                    {
                        r = await ws.ReceiveAsync(new ArraySegment<byte>(buffer), stop).ConfigureAwait(false);
                        if (r.MessageType == WebSocketMessageType.Close) { reason = ws.CloseStatusDescription ?? "closed"; goto closed; }
                        text.Append(Encoding.UTF8.GetString(buffer, 0, r.Count));
                    } while (!r.EndOfMessage);
                    Handle(text.ToString());
                }
            }
            catch (Exception ex) { reason = ex.Message; }
            closed:
            if (socket == ws && !stop.IsCancellationRequested)
            {
                socket = null;
                FailPending("Connection lost.");
                SocketClosed?.Invoke(reason);
            }
        }

        void Handle(string json)
        {
            JObject env;
            try { env = JObject.Parse(json); } catch (Exception) { return; }
            string cid = (string)env["cid"];
            if (env["match_data"] is JObject data)
            {
                string body = "";
                try { body = Encoding.UTF8.GetString(Convert.FromBase64String((string)data["data"] ?? "")); } catch (Exception) { }
                long op = 0; long.TryParse((string)data["op_code"] ?? "0", out op);
                inbox.Enqueue(new TangoMatchMessage { MatchId = (string)data["match_id"] ?? "", OpCode = op, Json = body });
                return;
            }
            if (!string.IsNullOrEmpty(cid) && pending.TryGetValue(cid, out var tcs))
            {
                if (env["error"] is JObject err) tcs.TrySetException(new InvalidOperationException((string)err["message"] ?? "Request failed."));
                else tcs.TrySetResult(env);
            }
        }

        void FailPending(string why)
        {
            foreach (var entry in pending)
                if (pending.TryRemove(entry.Key, out var tcs)) tcs.TrySetException(new InvalidOperationException(why));
        }

        public void Dispose() { _ = CloseSocket(); }
    }
}
