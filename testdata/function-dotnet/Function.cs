// The .NET fixture used to verify Google Functions Framework and Buildpacks
// support (#1319).
//
// It is an ordinary Functions Framework function: no CloudBurrow-specific
// code, because the point is that a normal function runs unchanged. It
// answers as testdata/function does.
using System;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using CloudNative.CloudEvents;
using Google.Cloud.Functions.Framework;
using Microsoft.AspNetCore.Http;

namespace Fixture;

// Hello answers an HTTP request, echoing a name when given one.
public class Hello : IHttpFunction
{
    public async Task HandleAsync(HttpContext context)
    {
        var name = "world";
        try
        {
            using var body = await JsonDocument.ParseAsync(context.Request.Body);
            if (body.RootElement.ValueKind == JsonValueKind.Object &&
                body.RootElement.TryGetProperty("name", out var given) &&
                given.ValueKind == JsonValueKind.String &&
                !string.IsNullOrEmpty(given.GetString()))
            {
                name = given.GetString()!;
            }
        }
        catch (JsonException)
        {
            // Not JSON: the default greeting.
        }
        context.Response.ContentType = "application/json";
        await context.Response.WriteAsync(JsonSerializer.Serialize(new { greeting = "hello " + name }));
    }
}

// Event receives a CloudEvent in the Google schema and logs it.
public class Event : ICloudEventFunction
{
    public Task HandleAsync(CloudEvent cloudEvent, CancellationToken cancellationToken)
    {
        var data = cloudEvent.Data switch
        {
            JsonElement json => json.GetRawText(),
            byte[] bytes => Encoding.UTF8.GetString(bytes),
            null => "",
            var other => other.ToString(),
        };
        Console.WriteLine($"received CloudEvent type={cloudEvent.Type} subject={cloudEvent.Subject} data={data}");
        return Task.CompletedTask;
    }
}
