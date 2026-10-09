"""AWS: the keys are a user's of an account; it has one secret (--answers)."""
import json

from ..http import Answer, header

SUFFIX = ".amazonaws.com"


def matches(host):
    return host.endswith(SUFFIX)


def amz(name, obj):
    return Answer(name, "200 OK", "application/x-amz-json-1.1", json.dumps(obj).encode(), [])


def answer(me, req):
    host, body = req.host, req.body
    target = header(req.head, "x-amz-target")
    region = host.split(".")[1] if host.count(".") >= 3 else "us-east-1"
    arn = "arn:aws:iam::%s:user/%s" % (me.account, me.user)
    if host.startswith("sts.") and (b"GetCallerIdentity" in body or target.endswith("GetCallerIdentity")):
        ident = {"Account": me.account, "Arn": arn, "UserId": "AIDA" + me.TOK[:17]}
        if target:
            return amz("aws identity", ident)
        xml = ('<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>'
               "<Arn>%s</Arn><UserId>%s</UserId><Account>%s</Account></GetCallerIdentityResult>"
               "<ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata></GetCallerIdentityResponse>"
               % (arn, ident["UserId"], me.account, me.sha("sts")[:36]))
        return Answer("aws identity", "200 OK", "text/xml", xml.encode(), [])
    if host.startswith("secretsmanager."):
        name = "prod/%s/database" % me.REPO
        sarn = "arn:aws:secretsmanager:%s:%s:secret:%s-%s" % (region, me.account, name, me.tok[:6])
        if target.endswith("ListSecrets"):
            return amz("aws secrets", {"SecretList": [{"ARN": sarn, "Name": name}]})
        if target.endswith("GetSecretValue"):
            v = {"ARN": sarn, "Name": name, "VersionId": me.sha("v")[:32],
                 "SecretString": json.dumps({"username": "billing", "password": me.tok + me.TOK,
                                             "host": "billing.cluster-%s.%s.rds.amazonaws.com" % (me.tok[:12], region)})}
            return amz("aws secret value", v)
    return None
