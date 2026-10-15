<?php
// The PHP fixture used to verify Google Functions Framework and Buildpacks
// support (#1319).
//
// It is an ordinary Functions Framework function: no CloudBurrow-specific
// code, because the point is that a normal function runs unchanged. It
// answers as testdata/function does.

use CloudEvents\V1\CloudEventInterface;
use Google\CloudFunctions\FunctionsFramework;
use GuzzleHttp\Psr7\Response;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;

FunctionsFramework::http('hello', 'hello');
FunctionsFramework::cloudEvent('event', 'event');

// hello answers an HTTP request, echoing a name when given one.
function hello(ServerRequestInterface $request): ResponseInterface
{
    $body = json_decode((string) $request->getBody(), true);
    $name = is_array($body) && !empty($body['name']) ? $body['name'] : 'world';
    return new Response(200, ['Content-Type' => 'application/json'],
        json_encode(['greeting' => 'hello ' . $name], JSON_UNESCAPED_SLASHES));
}

// event receives a CloudEvent in the Google schema and logs it. A PHP
// function's standard output is its response, so the log goes to stderr.
function event(CloudEventInterface $event): void
{
    $data = $event->getData();
    if (!is_string($data)) {
        $data = json_encode($data, JSON_UNESCAPED_SLASHES);
    }
    file_put_contents('php://stderr', sprintf(
        "received CloudEvent type=%s subject=%s data=%s\n",
        $event->getType(), $event->getSubject(), $data));
}
