import {
  aws_certificatemanager,
  aws_codebuild,
  aws_codepipeline,
  aws_codepipeline_actions,
  aws_ec2,
  aws_eks,
  aws_events_targets,
  aws_iam,
  aws_rds,
  aws_route53,
  aws_s3,
  aws_secretsmanager,
  aws_sns,
  aws_sns_subscriptions,
  CfnJson,
  CfnOutput,
  Stack,
  StackProps,
} from "aws-cdk-lib";
import { CodeStarConnectionsSourceAction } from "aws-cdk-lib/aws-codepipeline-actions";
import { Port } from "aws-cdk-lib/aws-ec2";
import { DatabaseClusterEngine } from "aws-cdk-lib/aws-rds";
import { Construct } from "constructs";
import * as fs from "fs";
import * as yaml from "js-yaml";
import * as path from "path";
import ebsCsiPolicyDocJson from "./iam_json/ebs_csi_driver_service_account.json";
import rwEksPolicyDocJson from "./iam_json/rw_eks_policy_document.json";

export class CkanCdkStack extends Stack {
  constructor(scope: Construct, id: string, props?: StackProps) {
    super(scope, id, props);

    //////////////////////////////////////////////////
    // CREATE & RETREIVE NETWORK_LEVEL INFRASTRUCTURE
    // INCLUDES EXISTING VPC, NEW TLS_CERT, PRIVATE AND PUBLIC SUBNETS
    //////////////////////////////////////////////////

    const TLS_CERT_NEW = new aws_certificatemanager.Certificate(
      this,
      "txwaterdatahubcert",
      {
        domainName: "*.txwaterdatahub.org",
        validation: aws_certificatemanager.CertificateValidation.fromDns(
          aws_route53.HostedZone.fromHostedZoneId(
            this,
            "txwaterdatahub_hostedzone",
            "Z0066498YS9I3BNY74D3"
          )
        ),
      }
    );
    
    const VPC = new aws_ec2.Vpc(this, "eks_vpc", {
      vpcName: "eks_vpc",
      maxAzs: 4,
    });

    //////////////////////////////////////////////////
    // AWS IAM INFRASTRUCTURE
    //////////////////////////////////////////////////

    //// retreive existing AdministratorAccess managed policy
    const administrator_access = aws_iam.ManagedPolicy.fromAwsManagedPolicyName(
      "AdministratorAccess"
    );
    //// create managed policy from policy document in ./iam_json
    const twdh_rw_eks_managed_policy = new aws_iam.ManagedPolicy(
      this,
      "twdh_rw_eks_managed_policy",
      {
        document: aws_iam.PolicyDocument.fromJson(rwEksPolicyDocJson),
        managedPolicyName: "twdh_rw_eks_managed_policy",
      }
    );
    
    //// codebuild role
    const principal = new aws_iam.ServicePrincipal("codebuild");
    const buildrole = new aws_iam.Role(this, "twdh_ckan_buildrole", {
      assumedBy: principal,
      managedPolicies: [twdh_rw_eks_managed_policy, administrator_access],
      roleName: "twdh_ckan_buildrole",
    });

    //////////////////////////////////////////////////
    // STORAGE RELATED INFRASTRUCTURE
    //////////////////////////////////////////////////

    //// S3 BUCKET FOR S3 FILESTORE
    const S3_FILESTORE_BUCKET = new aws_s3.Bucket(this, "twdh_s3_storage", {
      bucketName: "twdh-s3filestore",
      accessControl: aws_s3.BucketAccessControl.PUBLIC_READ,
    });

    const S3_FILESTORE_USER = new aws_iam.User(this, "S3_FILESTORE_USER", {
      userName: "TWDH_S3_FILESTORE_USER",
    });

    const S3_FILESTORE_USER_ACCESS_KEY = new aws_iam.AccessKey(
      this,
      "TWDH_S3_FILESTORE_USER_ACCESS_KEY",
      {
        user: S3_FILESTORE_USER,
      }
    );

    const S3FILESTORE_SECRET_STRING = JSON.stringify({
      ACCESS_KEY_ID: S3_FILESTORE_USER_ACCESS_KEY.accessKeyId.toString(),
      SECRET_ACCESS_KEY: S3_FILESTORE_USER_ACCESS_KEY.secretAccessKey.toString(),
    });

    const TWDH_S3FILESTORE_USER_SECRETS = new aws_secretsmanager.CfnSecret(
      this,
      "twdh_s3_filestore_user_secrets",
      {
        name: "twdh_s3_filestore_user_secrets",
        secretString: S3FILESTORE_SECRET_STRING,
      }
    );

    S3_FILESTORE_BUCKET.grantReadWrite(S3_FILESTORE_USER);
    //////////////////////////////////////////////////
    // EKS RELATED INFRASTRUCTURE
    //////////////////////////////////////////////////

    //// EKS CLUSTER
    const TWDH_KUBE_CLUSTER = new aws_eks.Cluster(this, "twdh_ckan_cluster", {
      clusterName: "twdh-kube-cluster",
      version: aws_eks.KubernetesVersion.V1_21,
      vpc: VPC,
      defaultCapacity: 2,
      defaultCapacityInstance: new aws_ec2.InstanceType("t3.large"),
      endpointAccess: aws_eks.EndpointAccess.PUBLIC_AND_PRIVATE,
    });
    //// TODO: ADD USER GROUPS VIA MANIFEST IN ./kube_manifests
    TWDH_KUBE_CLUSTER.awsAuth.addMastersRole(buildrole, "ci-cd-master");
    TWDH_KUBE_CLUSTER.awsAuth.addMastersRole(buildrole, "ci-cd-master1");
    TWDH_KUBE_CLUSTER.awsAuth.addUserMapping(
      aws_iam.User.fromUserName(
        this,
        "chris.repka",
        "chris.repka@twdb.texas.gov",
      ),
      {
        groups: ["eks_developer_group"],
      }
    );
    TWDH_KUBE_CLUSTER.awsAuth.addUserMapping(
      aws_iam.User.fromUserName(
        this,
        "ben.bright",
        "ben.bright@twdb.texas.gov",
      ),
      {
        groups: ["eks_developer_group"],
      }
    );
    //// TODO: ADD USERS MAPPINGS

    //// EKS Kube Manifests to apply on startup
    const manifestsDir = path.resolve("./lib/kube_manifests");
    const manifestsArray = fs.readdirSync(manifestsDir);
    const manifestsWithDir = manifestsArray.map((m) => `${manifestsDir}/${m}`);
    const manifests: any[] = [];
    manifestsWithDir.forEach((mwd) =>
      yaml.loadAll(fs.readFileSync(mwd, "utf-8"), (f) => manifests.push(f), {
        schema: yaml.JSON_SCHEMA,
      })
    );
    const KUBE_MANIFESTS = new aws_eks.KubernetesManifest(
      this,
      "twdh_kube_manifests",
      {
        cluster: TWDH_KUBE_CLUSTER,
        manifest: manifests,
        overwrite: true,
      }
    );

    /////////////////////////////////
    // EKS OIDC PROVIDER
    /////////////////////////////////

    //// create oidc provider
    const KUBE_OIDC_URL = TWDH_KUBE_CLUSTER.clusterOpenIdConnectIssuerUrl;
    const KUBE_OIDC_PROVIDER = new aws_iam.OpenIdConnectProvider(
      this,
      "TWDH_KUBE_OIDC",
      {
        url: KUBE_OIDC_URL,
        clientIds: ["sts.amazonaws.com"],
      }
    );

    ///////////////////////////////////////////////////////////
    // AWS SECRETS & CONFIG PROVIDER (ASCP) CSI DRIVER ADDON
    ///////////////////////////////////////////////////////////

    //// https://www.eksworkshop.com/beginner/194_secrets_manager/
    //// aws cli commands in above guide translated to cdk code below

    const OIDC_PRINCIPAL_CONDITIONS_ASCP = new CfnJson(
      this,
      "cfnJsonOIDCPrincipalASCP",
      {
        value: {
          [`${KUBE_OIDC_URL}:sub`]:
            "system:serviceaccount:default:secret-manager-service-account",
        },
      }
    );

    const OIDC_PRINCIPAL_ASCP = new aws_iam.FederatedPrincipal(
      KUBE_OIDC_PROVIDER.openIdConnectProviderArn,
      OIDC_PRINCIPAL_CONDITIONS_ASCP.toString,
      "sts:AssumeRoleWithWebIdentity"
    );

    const ASCP_SERVICE_ACCOUNT_ROLE = new aws_iam.Role(
      this,
      "ascp_service_account_role",
      {
        roleName: "twdh-ascp-service-account-role",
        assumedBy: OIDC_PRINCIPAL_ASCP,
      }
    );
    const ASCP_SERVICE_ACCOUNT_POLICY = new aws_iam.Policy(
      this,
      "ascp_service_account_policy",
      {
        statements: [
          new aws_iam.PolicyStatement({
            effect: aws_iam.Effect.ALLOW,
            actions: [
              "secretsmanager:GetSecretValue",
              "secretsmanager:DescribeSecret",
            ],
            resources: [
              //arn for db secrets with wildcard to account for auto-appended value
              "arn:aws:secretsmanager:us-east-1:746466009731:secret:twdh_*",
            ],
          }),
          new aws_iam.PolicyStatement({
            effect: aws_iam.Effect.ALLOW,
            actions: [
              "kms:GenerateDataKey",
              "kms:Decrypt",
              "sts:AssumeRoleWithWebIdentity",
            ],
            resources: ["*"],
          }),
        ],
      }
    );
    ASCP_SERVICE_ACCOUNT_POLICY.attachToRole(ASCP_SERVICE_ACCOUNT_ROLE);

    //// deploy aws secrets manager csi provider to cluster
    const ASCP_HELMCHART = new aws_eks.HelmChart(
      this,
      "secrets-store-csi-driver",
      {
        cluster: TWDH_KUBE_CLUSTER,
        repository:
          "https://kubernetes-sigs.github.io/secrets-store-csi-driver/charts",
        chart: "secrets-store-csi-driver",
        namespace: "kube-system",
        release: "secrets-store-csi-driver",
        values: {
          syncSecret: {
            enabled: "true",
          },
        },
      }
    );

    ///////////////////////////////////////////////////////////
    // AWS EBS CSI DRIVER CONFIG & SETUP ADDON
    ///////////////////////////////////////////////////////////

    //// helpful guide https://www.eksworkshop.com/beginner/170_statefulset/ebs_csi_driver/
    //// concepts from above translated into cdk commands/code from aws cli commands

    const OIDC_PRINCIPAL_CONDITIONS_EBSCSI = new CfnJson(
      this,
      "cfnJsonOIDCPrincipalEBSCSI",
      {
        value: {
          [`${KUBE_OIDC_URL}:sub`]:
            "system:serviceaccount:default:ebs-csi-driver-service-account",
        },
      }
    );

    const OIDC_PRINCIPAL_EBSCSI = new aws_iam.FederatedPrincipal(
      KUBE_OIDC_PROVIDER.openIdConnectProviderArn,
      OIDC_PRINCIPAL_CONDITIONS_EBSCSI.toString,
      "sts:AssumeRoleWithWebIdentity"
    );

    const EBS_CSI_ROLE_POLICY = new aws_iam.Policy(
      this,
      "EBS_CSI_ROLE_POLICY",
      {
        policyName: "twdh-ebs-csi-driver-role-policy",
        document: aws_iam.PolicyDocument.fromJson(ebsCsiPolicyDocJson),
      }
    );

    const EBS_CSI_ROLE = new aws_iam.Role(this, "EBS_CSI_ROLE", {
      roleName: "twdh-ebs-csi-driver-service-account-role",
      assumedBy: OIDC_PRINCIPAL_EBSCSI,
    });
    EBS_CSI_ROLE_POLICY.attachToRole(EBS_CSI_ROLE);

    const EBS_CSI_ADDON = new aws_eks.CfnAddon(this, "EBS_CSI_ADDON", {
      clusterName: TWDH_KUBE_CLUSTER.clusterName,
      addonName: "aws-ebs-csi-driver",
      serviceAccountRoleArn: EBS_CSI_ROLE.roleArn,
      addonVersion: "v1.6.1-eksbuild.1",
      resolveConflicts: "OVERWRITE",
    });

    //////////////////////////////////////////////////
    // AURORA DB INFRASTRUCTURE
    //////////////////////////////////////////////////

    //// Credentials for Aurora PG11 DB

    const db_creds = aws_rds.Credentials.fromGeneratedSecret("twdb_ckan", {
      secretName: "twdh_db",
      excludeCharacters: " %+~`#$&*,^()|[]{}:;<>?!'/@\"\\",
    });

    //// Aurora Postgres11 DB Created
    const aurora_db_cluster = new aws_rds.DatabaseCluster(this, "twdhdb", {
      clusterIdentifier: "twdhdb",
      defaultDatabaseName: "ckan_core",
      credentials: db_creds,
      instanceIdentifierBase: "twdhdb",
      instanceProps: {
        instanceType: new aws_ec2.InstanceType("t3.medium"),
        vpc: VPC,
        vpcSubnets: {
          subnets: VPC.privateSubnets,
        },
      },
      engine: DatabaseClusterEngine.auroraPostgres({
        version: aws_rds.AuroraPostgresEngineVersion.VER_11_11,
      }),
    });

    aurora_db_cluster.connections.allowFrom(
      TWDH_KUBE_CLUSTER,
      Port.tcp(5432),
      "allow incoming on 5432 from eks cluster security group"
    );

    //////////////////////////////////////////////////
    // CODEBUILD CDK CONSTRUCTS
    //////////////////////////////////////////////////

    //// codebuild project constructs
    const twdh_codebuild_project_dev = new aws_codebuild.Project(
      this,
      "twdh_codebuild_project_dev",
      {
        projectName: "twdh_codebuild_project_dev",
        role: buildrole,
        environment: {
          buildImage: aws_codebuild.LinuxBuildImage.AMAZON_LINUX_2_3,
          privileged: true,
        },
        buildSpec: aws_codebuild.BuildSpec.fromObject({
          version: "0.2",
          phases: {
            pre_build: {
              commands: [
                "export KUBECONFIG=~/.kube/config",
                "export TAG=${CODEBUILD_RESOLVED_SOURCE_VERSION}",
                "env",
                "docker version",
                "docker ps",
                "node --version",
                "python --version",
                "sudo mkdir /usr/local/awscliv2",
                "curl https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o awscliv2.zip",
                "unzip awscliv2.zip",
                "sudo ./aws/install --update --bin-dir /usr/local/bin --install-dir /usr/local/awscliv2",
                'export PATH="/usr/local/bin:$PATH"',
                "aws --version",
                "curl -L https://git.io/get_helm.sh | bash -s -- --version v3.8.2",
                "helm version",
                "aws sts get-caller-identity",
              ],
            },
            build: {
              commands: [
                "echo @@@ BEGIN BUILD STAGE @@@",
                "aws --version",
                //"kubectl version",
                //"sudo cat /root/.kube/config",
                "aws-iam-authenticator version",
                //"python ./deployment_scripts/dev_db.py",
                "python ./deployment_scripts/blue_green.py dev",
                "kubectl get pods",
              ],
            },
          },
        }),
      }
    );

    //// codebuild instruction set / project for prod
    const twdh_codebuild_project_prod = new aws_codebuild.Project(
      this,
      "twdh_codebuild_project_prod",
      {
        projectName: "twdh_codebuild_project_prod",
        role: buildrole,
        environment: {
          buildImage: aws_codebuild.LinuxBuildImage.AMAZON_LINUX_2_3,
          privileged: true,
        },
        buildSpec: aws_codebuild.BuildSpec.fromObject({
          version: "0.2",
          phases: {
            pre_build: {
              commands: [
                "export KUBECONFIG=~/.kube/config",
                "export TAG=${CODEBUILD_RESOLVED_SOURCE_VERSION}",
                "env",
                "docker version",
                "docker ps",
                "node --version",
                "python --version",
                "sudo mkdir /usr/local/awscliv2",
                "curl https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o awscliv2.zip",
                "unzip awscliv2.zip",
                "sudo ./aws/install --update --bin-dir /usr/local/bin --install-dir /usr/local/awscliv2",
                'export PATH="/usr/local/bin:$PATH"',
                "aws --version",
                "curl -L https://git.io/get_helm.sh | bash -s -- --version v3.8.2",
                "helm version",
                "aws sts get-caller-identity",
              ],
            },
            build: {
              commands: [
                "echo @@@ BEGIN BUILD STAGE @@@",
                "aws --version",
                //"kubectl version",
                //"sudo cat /root/.kube/config",
                "aws-iam-authenticator version",
                //"python ./deployment_scripts/dev_db.py",
                "python ./deployment_scripts/blue_green.py prod",
                "kubectl get pods",
              ],
            },
          },
        }),
      }
    );

    //////////////////////////////////////////////////
    // CODEPIPELINE CDK CONSTRUCTS
    //////////////////////////////////////////////////

    //// source action constructs
    const sourceOutput = new aws_codepipeline.Artifact();
    const sourceAction = new CodeStarConnectionsSourceAction({
      actionName: "GitHub_Source",
      branch: "main",
      owner: "TNRIS",
      repo: "twdh_CKAN_monorepo",
      connectionArn:
        "arn:aws:codestar-connections:us-east-1:746466009731:connection/7ac900c8-fcdf-4c4f-a6c2-57650254c1c0",
      output: sourceOutput,
    });

    //// build action constructs
    const buildOutputDev = new aws_codepipeline.Artifact();
    const buildDevAction = new aws_codepipeline_actions.CodeBuildAction({
      actionName: "TwdhBuildDevAction",
      project: twdh_codebuild_project_dev,
      input: sourceOutput,
      outputs: [buildOutputDev],
    });
    const buildOutputProd = new aws_codepipeline.Artifact();
    const buildProdAction = new aws_codepipeline_actions.CodeBuildAction({
      actionName: "TwdhBuildDevAction",
      project: twdh_codebuild_project_prod,
      input: sourceOutput,
      outputs: [buildOutputProd],
    });

    //// approval action constructs
    const approvalAction = new aws_codepipeline_actions.ManualApprovalAction({
      actionName: "Approve",
      notifyEmails: ["chris.repka@twdb.texas.gov", "ben.bright@twdb.texas.gov"],
      externalEntityLink: "https://dev.txwaterdatahub.org",
    });

    //// codepipeline core construct
    const codepipeline = new aws_codepipeline.Pipeline(
      this,
      "twdh_ckan_pipeline",
      {
        pipelineName: "twdh_ckan_pipeline",
        stages: [
          {
            stageName: "Source",
            actions: [sourceAction],
          },
          {
            stageName: "BuildDevRelease",
            actions: [buildDevAction],
          },
          {
            stageName: "Approval",
            actions: [approvalAction],
          },
          {
            stageName: "BuildProdRelease",
            actions: [buildProdAction],
          },
        ],
      }
    );

    //////////////////////////////////////////////////
    // EXTRA BUILD NOTIFICATIONS
    //////////////////////////////////////////////////

    //// pre-create subscription to assign to topic
    const chris_subscription = new aws_sns_subscriptions.EmailSubscription(
      "chris.repka@twdb.texas.gov"
    );
    //// pre-create subscription to assign to topic
    const ben_subscription = new aws_sns_subscriptions.EmailSubscription(
      "ben.bright@twdb.texas.gov"
    );
    //// create topic
    const build_notify_topic = new aws_sns.Topic(
      this,
      "twdh_ckan_build_topic",
      {
        displayName: "twdh_ckan_cicd_builds",
        topicName: "twdh-ckan-cicd-builds",
      }
    );
    //// assign subscriptions to topic
    build_notify_topic.addSubscription(chris_subscription);
    build_notify_topic.addSubscription(ben_subscription);
    //// notify on dev failure
    twdh_codebuild_project_dev.onBuildFailed("twdh_ckan_dev_build_failed", {
      target: new aws_events_targets.SnsTopic(build_notify_topic),
    });
    //// notify on prod failure
    twdh_codebuild_project_prod.onBuildFailed("twdh_ckan_prod_build_failed", {
      target: new aws_events_targets.SnsTopic(build_notify_topic),
    });
    //// notify on prod success
    twdh_codebuild_project_prod.onBuildSucceeded(
      "twdh_ckan_prod_build_succeeded",
      {
        target: new aws_events_targets.SnsTopic(build_notify_topic),
      }
    );
  }
}